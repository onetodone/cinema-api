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

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/service/booking"
)

// The tests run in synctest bubbles: time is fake and only moves when every goroutine of the test waits, so
// a test can sleep for minutes of worker time in microseconds and observe exactly when each sweep ran.

const (
	testInterval  = 5 * time.Second
	testBatchSize = 3
)

// fakeBookings is a BookingExpirer whose answers the test scripts. It records the fake time of every call.
type fakeBookings struct {
	mu    sync.Mutex
	calls []time.Time
	// expire answers the n-th call, counting from 1. Without it, every batch comes back empty.
	expire func(ctx context.Context, n int) (booking.ExpiredBatch, error)
}

func (f *fakeBookings) ExpireBatch(ctx context.Context, limit int) (booking.ExpiredBatch, error) {
	f.mu.Lock()
	f.calls = append(f.calls, time.Now())
	n := len(f.calls)
	f.mu.Unlock()

	if limit != testBatchSize {
		return booking.ExpiredBatch{}, errors.New("unexpected limit")
	}
	if f.expire == nil {
		return booking.ExpiredBatch{}, nil
	}
	return f.expire(ctx, n)
}

func (f *fakeBookings) callTimes() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// batchOf returns a batch of n expired bookings with one seat each.
func batchOf(n int) booking.ExpiredBatch {
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.NewV7()
	}
	return booking.ExpiredBatch{BookingIDs: ids, Seats: int64(n)}
}

// logBuffer collects log output; the worker goroutine writes while the test reads.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// runExpirer starts an Expirer over f in the background and returns a function that cancels it and waits for
// Run to return. Call it inside a synctest bubble.
func runExpirer(t *testing.T, f *fakeBookings, logs io.Writer) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	e := NewExpirer(f, ExpirerConfig{Interval: testInterval, BatchSize: testBatchSize}, slog.New(slog.NewTextHandler(logs, nil)))
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

func TestExpirerSweepsAtStartAndThenAboutEveryInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeBookings{}
		began := time.Now()
		stop := runExpirer(t, f, io.Discard)
		time.Sleep(20 * testInterval)
		stop()

		calls := f.callTimes()
		// 100 s of pauses between 4 and 6 s each.
		if len(calls) < 17 || len(calls) > 26 {
			t.Fatalf("%d sweeps in %s, want 17 to 26", len(calls), 20*testInterval)
		}
		if !calls[0].Equal(began) {
			t.Errorf("first sweep after %s, want one at start", calls[0].Sub(began))
		}
		pauses := map[time.Duration]bool{}
		for i := 1; i < len(calls); i++ {
			pause := calls[i].Sub(calls[i-1])
			if pause < testInterval*8/10 || pause > testInterval*12/10 {
				t.Errorf("pause %d = %s, want the %s interval ±20%%", i, pause, testInterval)
			}
			pauses[pause] = true
		}
		if len(pauses) < 2 {
			t.Errorf("all pauses are %v; want them jittered", pauses)
		}
	})
}

func TestExpirerDrainsFullBatchesWithinOneSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeBookings{expire: func(_ context.Context, n int) (booking.ExpiredBatch, error) {
			switch n {
			case 1, 2:
				return batchOf(testBatchSize), nil // full: more may be due
			case 3:
				return batchOf(1), nil // short: nothing else was due
			}
			return booking.ExpiredBatch{}, nil
		}}
		var logs logBuffer
		began := time.Now()
		stop := runExpirer(t, f, &logs)

		synctest.Wait() // the first sweep is over, and the loop waits for the next one
		calls := f.callTimes()
		if len(calls) != 3 || !calls[2].Equal(began) {
			t.Fatalf("first sweep made %d calls, the last after %s; want 3 at once", len(calls), calls[len(calls)-1].Sub(began))
		}

		time.Sleep(testInterval * 12 / 10)
		stop()
		if n := len(f.callTimes()); n != 4 {
			t.Errorf("%d calls after the second sweep, want 4", n)
		}
		out := logs.String()
		if strings.Count(out, `msg="bookings expired" job=expirer bookings=3 seats=3`) != 2 ||
			strings.Count(out, `msg="bookings expired" job=expirer bookings=1 seats=1`) != 1 ||
			strings.Count(out, "bookings expired") != 3 {
			t.Errorf("want one log line per non-empty batch:\n%s", out)
		}
	})
}

func TestExpirerRetriesFailedBatchesAtTheNextSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeBookings{expire: func(_ context.Context, n int) (booking.ExpiredBatch, error) {
			switch n {
			case 1:
				return booking.ExpiredBatch{}, domain.Busy(domain.CodeSeatBusy, "the seats are locked")
			case 2:
				return booking.ExpiredBatch{}, errors.New("connection refused")
			case 3:
				return batchOf(2), nil
			}
			return booking.ExpiredBatch{}, nil
		}}
		var logs logBuffer
		stop := runExpirer(t, f, &logs)

		synctest.Wait()
		if n := len(f.callTimes()); n != 1 {
			t.Fatalf("%d calls in the first sweep, want 1: a failed batch ends the sweep", n)
		}
		time.Sleep(2 * testInterval * 12 / 10) // at least two more sweeps
		stop()

		if n := len(f.callTimes()); n < 3 {
			t.Fatalf("%d calls, want the failed batches retried by later sweeps", n)
		}
		out := logs.String()
		for _, want := range []string{
			`level=WARN msg="expiry batch gave up waiting for a lock; the next sweep retries it" job=expirer error="the seats are locked"`,
			`level=ERROR msg="expiry batch failed; the next sweep retries it" job=expirer error="connection refused"`,
			`level=INFO msg="bookings expired" job=expirer bookings=2 seats=2`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("log lacks %q:\n%s", want, out)
			}
		}
	})
}

func TestExpirerFinishesTheBatchInFlightOnShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var (
			release  = make(chan struct{})
			batchErr error // the batch context's error when the batch finished
		)
		f := &fakeBookings{expire: func(ctx context.Context, n int) (booking.ExpiredBatch, error) {
			if n > 1 {
				return booking.ExpiredBatch{}, nil
			}
			<-release
			batchErr = ctx.Err()
			return batchOf(testBatchSize), nil // a full batch: without the shutdown, another would follow
		}}
		ctx, cancel := context.WithCancel(t.Context())
		e := NewExpirer(f, ExpirerConfig{Interval: testInterval, BatchSize: testBatchSize}, slog.New(slog.DiscardHandler))
		done := make(chan struct{})
		go func() {
			defer close(done)
			e.Run(ctx)
		}()

		synctest.Wait() // the first batch is in flight
		cancel()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("Run returned while a batch was in flight")
		default:
		}

		close(release)
		<-done
		if batchErr != nil {
			t.Errorf("the batch in flight was canceled: %v", batchErr)
		}
		if n := len(f.callTimes()); n != 1 {
			t.Errorf("%d batches, want no new one after shutdown began", n)
		}
	})
}

func TestExpirerGivesUpOnAHungBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeBookings{expire: func(ctx context.Context, n int) (booking.ExpiredBatch, error) {
			if n > 1 {
				return booking.ExpiredBatch{}, nil
			}
			<-ctx.Done() // a database that never answers
			return booking.ExpiredBatch{}, ctx.Err()
		}}
		var logs logBuffer
		began := time.Now()
		stop := runExpirer(t, f, &logs)

		time.Sleep(batchTimeout + testInterval*12/10)
		stop()

		calls := f.callTimes()
		if len(calls) < 2 {
			t.Fatalf("%d calls, want the loop to go on after the hung batch", len(calls))
		}
		if next := calls[1].Sub(began); next < batchTimeout+testInterval*8/10 {
			t.Errorf("second sweep after %s, want one interval after the %s batch timeout", next, batchTimeout)
		}
		if !strings.Contains(logs.String(), `level=ERROR msg="expiry batch failed; the next sweep retries it" job=expirer error="context deadline exceeded"`) {
			t.Errorf("the timeout was not logged:\n%s", logs.String())
		}
	})
}

func TestExpirerDoesNotSweepAfterCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeBookings{}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		NewExpirer(f, ExpirerConfig{Interval: testInterval, BatchSize: testBatchSize}, slog.New(slog.DiscardHandler)).Run(ctx)
		if n := len(f.callTimes()); n != 0 {
			t.Errorf("%d sweeps with a canceled context, want none", n)
		}
	})
}

func TestNewExpirerRejectsAnInvalidConfig(t *testing.T) {
	t.Parallel()

	for _, cfg := range []ExpirerConfig{{Interval: 0, BatchSize: 1}, {Interval: time.Second, BatchSize: 0}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewExpirer(%+v) did not panic", cfg)
				}
			}()
			NewExpirer(&fakeBookings{}, cfg, slog.New(slog.DiscardHandler))
		}()
	}
}

func TestJitteredStaysWithinTwentyPercent(t *testing.T) {
	t.Parallel()

	const d = 10 * time.Second
	lowest, highest := d, d
	for range 1000 {
		j := jittered(d)
		if j < 8*time.Second || j > 12*time.Second {
			t.Fatalf("jittered(%s) = %s, want 8s to 12s", d, j)
		}
		lowest, highest = min(lowest, j), max(highest, j)
	}
	if lowest > 9*time.Second || highest < 11*time.Second {
		t.Errorf("1000 samples spread only from %s to %s", lowest, highest)
	}
	if got := jittered(time.Nanosecond); got != time.Nanosecond {
		t.Errorf("jittered(1ns) = %s, want it unchanged", got)
	}
}
