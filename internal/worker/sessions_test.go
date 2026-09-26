package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

const testSweepInterval = time.Hour

// fakeSessions is an ExpiredSessions whose answers the test scripts. It records the fake time of every call.
type fakeSessions struct {
	mu    sync.Mutex
	calls []time.Time
	// deleteExpired answers the n-th call, counting from 1. Without it, nothing has expired.
	deleteExpired func(ctx context.Context, n int) (int, error)
}

func (f *fakeSessions) DeleteExpired(ctx context.Context, limit int) (int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, time.Now())
	n := len(f.calls)
	f.mu.Unlock()

	if limit != testBatchSize {
		return 0, errors.New("unexpected limit")
	}
	if f.deleteExpired == nil {
		return 0, nil
	}
	return f.deleteExpired(ctx, n)
}

func (f *fakeSessions) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func TestSessionSweeperDrainsFullBatchesAndRetriesFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeSessions{deleteExpired: func(_ context.Context, n int) (int, error) {
			switch n {
			case 1, 2:
				return testBatchSize, nil // full: more may have expired
			case 3:
				return 1, nil // short: that was all
			case 4:
				return 0, errors.New("connection refused")
			case 5:
				return 2, nil
			}
			return 0, nil
		}}
		var logs logBuffer
		m := newMetrics()
		s := NewSessionSweeper(f, SweeperConfig{Interval: testSweepInterval, BatchSize: testBatchSize}, m,
			slog.New(slog.NewTextHandler(&logs, nil)))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.Run(ctx)
		}()

		synctest.Wait()
		if n := f.callCount(); n != 3 {
			t.Fatalf("the first sweep made %d calls, want 3: full batches, then a short one", n)
		}
		time.Sleep(2 * testSweepInterval * 12 / 10) // at least two more sweeps
		cancel()
		<-done

		out := logs.String()
		for _, want := range []string{
			`level=INFO msg="expired sessions deleted" job=session_sweeper sessions=7`,
			`level=ERROR msg="deleting expired sessions failed; the next sweep retries" job=session_sweeper error="connection refused"`,
			`level=INFO msg="expired sessions deleted" job=session_sweeper sessions=2`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("log lacks %q:\n%s", want, out)
			}
		}
		if n := testutil.ToFloat64(m.SessionsSwept); n != 2*testBatchSize+1+2 {
			t.Errorf("cinema_sessions_swept_total = %v, want %d", n, 2*testBatchSize+1+2)
		}
		if strings.Count(out, "expired sessions deleted") != 2 {
			t.Errorf("want one line per sweep that deleted something:\n%s", out)
		}
	})
}

func TestSessionSweeperFinishesTheBatchInFlightOnShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var (
			release  = make(chan struct{})
			batchErr error
		)
		f := &fakeSessions{deleteExpired: func(ctx context.Context, n int) (int, error) {
			if n > 1 {
				return 0, nil
			}
			<-release
			batchErr = ctx.Err()
			return testBatchSize, nil // a full batch: without the shutdown, another would follow
		}}
		s := NewSessionSweeper(f, SweeperConfig{Interval: testSweepInterval, BatchSize: testBatchSize}, newMetrics(),
			slog.New(slog.DiscardHandler))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.Run(ctx)
		}()

		synctest.Wait()
		cancel()
		close(release)
		<-done
		if batchErr != nil {
			t.Errorf("the batch in flight was canceled: %v", batchErr)
		}
		if n := f.callCount(); n != 1 {
			t.Errorf("%d batches, want no new one after shutdown began", n)
		}
	})
}

func TestNewSessionSweeperRejectsBadSettings(t *testing.T) {
	t.Parallel()

	for _, cfg := range []SweeperConfig{{Interval: 0, BatchSize: 1}, {Interval: time.Minute, BatchSize: 0}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewSessionSweeper(%+v) did not panic", cfg)
				}
			}()
			NewSessionSweeper(&fakeSessions{}, cfg, newMetrics(), slog.New(slog.DiscardHandler))
		}()
	}
}
