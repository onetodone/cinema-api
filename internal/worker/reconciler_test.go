package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/service/booking"
)

const testReconcileBatch = 2

// reconcileCall is one ReconcileBatch call: when it happened, and where its batch started.
type reconcileCall struct {
	at    time.Time
	after uuid.UUID
}

// fakePayments is a PaymentReconciler whose answers the test scripts. Without a script, every batch is empty.
type fakePayments struct {
	mu    sync.Mutex
	calls []reconcileCall
	// reconcile answers the n-th call, counting from 1.
	reconcile func(ctx context.Context, n int, after uuid.UUID) (booking.ReconciledBatch, error)
}

func (f *fakePayments) ReconcileBatch(ctx context.Context, afterID uuid.UUID, limit int) (booking.ReconciledBatch, error) {
	f.mu.Lock()
	f.calls = append(f.calls, reconcileCall{at: time.Now(), after: afterID})
	n := len(f.calls)
	f.mu.Unlock()

	if limit != testReconcileBatch {
		return booking.ReconciledBatch{}, errors.New("unexpected limit")
	}
	if f.reconcile == nil {
		return booking.ReconciledBatch{LastID: afterID}, nil
	}
	return f.reconcile(ctx, n, afterID)
}

func (f *fakePayments) recorded() []reconcileCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// runReconciler starts a Reconciler over f in the background and returns a function that cancels it and waits
// for Run to return, and the metrics the Reconciler records. Call it inside a synctest bubble.
func runReconciler(t *testing.T, f *fakePayments, logs io.Writer) (stop func(), m *metrics.Metrics) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	m = newMetrics()
	r := NewReconciler(f, ReconcilerConfig{Interval: testInterval, BatchSize: testReconcileBatch}, m,
		slog.New(slog.NewTextHandler(logs, nil)))
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}, m
}

func TestReconcilerPassesAtStartAndThenAboutEveryInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakePayments{}
		began := time.Now()
		stop, _ := runReconciler(t, f, io.Discard)
		time.Sleep(10 * testInterval)
		stop()

		calls := f.recorded()
		if len(calls) < 8 || len(calls) > 13 {
			t.Fatalf("%d passes in %s, want 8 to 13", len(calls), 10*testInterval)
		}
		if !calls[0].at.Equal(began) {
			t.Errorf("first pass after %s, want one at start", calls[0].at.Sub(began))
		}
		for i := 1; i < len(calls); i++ {
			if pause := calls[i].at.Sub(calls[i-1].at); pause < testInterval*8/10 || pause > testInterval*12/10 {
				t.Errorf("pause %d = %s, want the %s interval ±20%%", i, pause, testInterval)
			}
		}
	})
}

// TestReconcilerWorksThroughEveryStuckPayment: a pass continues after the last payment of each full batch, logs
// every batch that settled something, and the next pass starts from the beginning again.
func TestReconcilerWorksThroughEveryStuckPayment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := uuid.NewV7(), uuid.NewV7()
		f := &fakePayments{reconcile: func(_ context.Context, n int, after uuid.UUID) (booking.ReconciledBatch, error) {
			switch n {
			case 1:
				return booking.ReconciledBatch{Checked: 2, LastID: first, Paid: 1, Failed: 1}, nil
			case 2:
				// One payment was settled by someone else meanwhile: checked, but not counted.
				return booking.ReconciledBatch{Checked: 2, LastID: second}, nil
			case 3:
				return booking.ReconciledBatch{Checked: 1, LastID: uuid.NewV7(), Failed: 1}, nil
			}
			return booking.ReconciledBatch{LastID: after}, nil
		}}
		var logs logBuffer
		stop, m := runReconciler(t, f, &logs)
		synctest.Wait()

		calls := f.recorded()
		if len(calls) != 3 || calls[0].after != (uuid.UUID{}) || calls[1].after != first || calls[2].after != second {
			t.Fatalf("first pass = %+v, want 3 batches, each after the last payment of the one before", calls)
		}
		time.Sleep(testInterval * 12 / 10)
		stop()
		if calls := f.recorded(); len(calls) != 4 || calls[3].after != (uuid.UUID{}) {
			t.Errorf("second pass = %+v, want it to start from the beginning", calls[3:])
		}

		out := logs.String()
		for _, want := range []string{
			`level=INFO msg="stuck payments settled" job=reconciler checked=2 paid=1 failed=1`,
			`level=INFO msg="stuck payments settled" job=reconciler checked=1 paid=0 failed=1`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("log lacks %q:\n%s", want, out)
			}
		}
		if n := strings.Count(out, "stuck payments settled"); n != 2 {
			t.Errorf("%d log lines, want one per batch that settled something:\n%s", n, out)
		}
		for result, want := range map[string]float64{metrics.ReconciledPaid: 1, metrics.ReconciledFailed: 2, metrics.ReconciledUnsettled: 0} {
			if n := testutil.ToFloat64(m.PaymentsReconciled.WithLabelValues(result)); n != want {
				t.Errorf("cinema_payments_reconciled_total{result=%q} = %v, want %v", result, n, want)
			}
		}
	})
}

func TestReconcilerLogsFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stuck := uuid.NewV7()
		f := &fakePayments{reconcile: func(_ context.Context, n int, after uuid.UUID) (booking.ReconciledBatch, error) {
			switch n {
			case 1:
				// Some payments of a full batch failed: the pass still goes on after them.
				return booking.ReconciledBatch{Checked: 2, LastID: stuck, Failed: 1, Unsettled: 1},
					errors.New("payment x: ask payment provider local: unreachable")
			case 2:
				return booking.ReconciledBatch{LastID: after}, errors.New("connection refused")
			}
			return booking.ReconciledBatch{LastID: after}, nil
		}}
		var logs logBuffer
		stop, m := runReconciler(t, f, &logs)
		synctest.Wait()
		stop()

		if n := testutil.ToFloat64(m.PaymentsReconciled.WithLabelValues(metrics.ReconciledUnsettled)); n != 1 {
			t.Errorf("unsettled payments counted = %v, want 1", n)
		}
		if calls := f.recorded(); len(calls) != 2 || calls[1].after != stuck {
			t.Errorf("calls = %+v, want the pass to go on after the failed payments", calls)
		}
		out := logs.String()
		for _, want := range []string{
			`level=WARN msg="some stuck payments could not be settled; the next pass retries them" job=reconciler error="payment x: ask payment provider local: unreachable"`,
			`level=INFO msg="stuck payments settled" job=reconciler checked=2 paid=0 failed=1`,
			`level=ERROR msg="listing stuck payments failed; the next pass retries" job=reconciler error="connection refused"`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("log lacks %q:\n%s", want, out)
			}
		}
	})
}

// TestReconcilerStopsQuietly: a listing cut short by shutdown is not an error worth logging.
func TestReconcilerStopsQuietly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		f := &fakePayments{reconcile: func(ctx context.Context, _ int, after uuid.UUID) (booking.ReconciledBatch, error) {
			cancel()
			return booking.ReconciledBatch{LastID: after}, ctx.Err()
		}}
		var logs logBuffer
		NewReconciler(f, ReconcilerConfig{Interval: testInterval, BatchSize: testReconcileBatch}, newMetrics(),
			slog.New(slog.NewTextHandler(&logs, nil))).Run(ctx)

		if n := len(f.recorded()); n != 1 {
			t.Errorf("%d calls, want 1", n)
		}
		if logs.String() != "" {
			t.Errorf("shutdown was logged:\n%s", logs.String())
		}
	})
}

func TestNewReconcilerRejectsAnInvalidConfig(t *testing.T) {
	t.Parallel()

	for _, cfg := range []ReconcilerConfig{{Interval: 0, BatchSize: 1}, {Interval: time.Second, BatchSize: 0}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewReconciler(%+v) did not panic", cfg)
				}
			}()
			NewReconciler(&fakePayments{}, cfg, newMetrics(), slog.New(slog.DiscardHandler))
		}()
	}
}
