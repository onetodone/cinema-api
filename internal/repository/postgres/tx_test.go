package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/onetodone/cinema-api/internal/platform/metrics"
)

func newMetrics() *metrics.Metrics {
	return metrics.New(prometheus.NewRegistry())
}

// failing returns an attempt function that fails with the given SQLSTATEs, one per call, and then succeeds.
func failing(calls *int, codes ...string) func() error {
	return func() error {
		*calls++
		if *calls <= len(codes) {
			return fmt.Errorf("lock seats: %w", &pgconn.PgError{Code: codes[*calls-1]})
		}
		return nil
	}
}

func TestRetryRerunsDeadlocksAndSerializationFailures(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	calls := 0

	m := newMetrics()
	err := retry(t.Context(), logger, m, failing(&calls, sqlstateDeadlockDetected, sqlstateSerializationFailure))
	if err != nil {
		t.Fatalf("retry = %v, want success on the third attempt", err)
	}
	if calls != 3 {
		t.Errorf("attempts = %d, want 3", calls)
	}
	if n := strings.Count(logs.String(), "retrying"); n != 2 {
		t.Errorf("logged %d retries, want 2:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "sqlstate=40P01") || !strings.Contains(logs.String(), "sqlstate=40001") {
		t.Errorf("retry logs do not name the SQLSTATE:\n%s", logs.String())
	}
	for _, code := range []string{metrics.SQLStateDeadlock, metrics.SQLStateSerializationFailure} {
		if n := testutil.ToFloat64(m.TxRetries.WithLabelValues(code)); n != 1 {
			t.Errorf("retries with SQLSTATE %s = %v, want 1", code, n)
		}
	}
}

func TestRetryGivesUpAfterMaxAttempts(t *testing.T) {
	t.Parallel()

	calls := 0
	codes := make([]string, maxTxAttempts+1)
	for i := range codes {
		codes[i] = sqlstateDeadlockDetected
	}

	err := retry(t.Context(), slog.New(slog.DiscardHandler), newMetrics(), failing(&calls, codes...))
	if pgErrorCode(err) != sqlstateDeadlockDetected {
		t.Errorf("retry = %v, want the last deadlock error", err)
	}
	if calls != maxTxAttempts {
		t.Errorf("attempts = %d, want %d", calls, maxTxAttempts)
	}
}

func TestRetryReturnsOtherErrorsAtOnce(t *testing.T) {
	t.Parallel()

	for _, code := range []string{sqlstateUniqueViolation, sqlstateLockNotAvailable} {
		calls := 0
		err := retry(t.Context(), slog.New(slog.DiscardHandler), newMetrics(), failing(&calls, code))
		if pgErrorCode(err) != code || calls != 1 {
			t.Errorf("SQLSTATE %s: err %v after %d attempts, want it returned after 1", code, err, calls)
		}
	}

	calls := 0
	plain := errors.New("not a database error")
	err := retry(t.Context(), slog.New(slog.DiscardHandler), newMetrics(), func() error { calls++; return plain })
	if !errors.Is(err, plain) || calls != 1 {
		t.Errorf("plain error: %v after %d attempts", err, calls)
	}
}

func TestRetryStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0

	err := retry(ctx, slog.New(slog.DiscardHandler), newMetrics(), failing(&calls, sqlstateDeadlockDetected, sqlstateDeadlockDetected))
	if pgErrorCode(err) != sqlstateDeadlockDetected || calls != 1 {
		t.Errorf("err %v after %d attempts, want the first error and no retry", err, calls)
	}
}
