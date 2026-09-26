package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/onetodone/cinema-api/internal/platform/metrics"
)

// DefaultSweepBatchSize is how many expired sessions one statement deletes. It bounds the rows one statement locks.
const DefaultSweepBatchSize = 1000

// ExpiredSessions deletes expired sessions. It is implemented by repository/postgres.Sessions directly: deleting
// what has expired has no rule a use case would add, and the session use cases need the token signing key, which
// the worker does not have.
type ExpiredSessions interface {
	// DeleteExpired deletes up to limit expired sessions and returns how many it deleted. Sessions that another
	// sweeper holds are skipped.
	DeleteExpired(ctx context.Context, limit int) (int, error)
}

// SweeperConfig configures a SessionSweeper.
type SweeperConfig struct {
	Interval  time.Duration // average pause between two sweeps
	BatchSize int           // most sessions deleted per statement
}

// SessionSweeper deletes expired sessions. An expired session cannot refresh whether or not it has been deleted,
// and a refresh deletes the one it finds expired, so the sweeper only keeps the table from growing with sessions
// whose clients never came back. Any number of sweepers may run at once; the database hands every expired session
// to one of them.
type SessionSweeper struct {
	sessions ExpiredSessions
	cfg      SweeperConfig
	metrics  *metrics.Metrics
	logger   *slog.Logger
}

// NewSessionSweeper returns a SessionSweeper that counts the sessions it deletes in m. It panics if the interval
// or the batch size is not positive.
func NewSessionSweeper(sessions ExpiredSessions, cfg SweeperConfig, m *metrics.Metrics, logger *slog.Logger) *SessionSweeper {
	if cfg.Interval <= 0 || cfg.BatchSize <= 0 {
		panic(fmt.Sprintf("worker: session sweeper interval and batch size must be positive, got %s and %d",
			cfg.Interval, cfg.BatchSize))
	}
	return &SessionSweeper{sessions: sessions, cfg: cfg, metrics: m, logger: logger.With(slog.String("job", "session_sweeper"))}
}

// Run sweeps at once and then about every interval until ctx is canceled. A failed batch is logged and retried by
// the next sweep. Once ctx is canceled, Run lets the batch in flight finish, starts no other one, and returns.
func (s *SessionSweeper) Run(ctx context.Context) {
	every(ctx, s.cfg.Interval, s.sweep)
}

// sweep deletes expired sessions batch by batch until a batch comes back short, a batch fails, or ctx is canceled.
func (s *SessionSweeper) sweep(ctx context.Context) {
	start := time.Now()
	deleted := 0
	for ctx.Err() == nil {
		n, err := s.deleteBatch(ctx)
		deleted += n
		if err != nil {
			s.logger.ErrorContext(ctx, "deleting expired sessions failed; the next sweep retries", slog.Any("error", err))
			break
		}
		if n < s.cfg.BatchSize {
			break
		}
	}
	if deleted > 0 {
		s.logger.InfoContext(ctx, "expired sessions deleted",
			slog.Int("sessions", deleted),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()))
	}
}

func (s *SessionSweeper) deleteBatch(ctx context.Context) (int, error) {
	// Like an expiry batch, a batch in flight is not cut off by shutdown: it is one short statement.
	batchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), batchTimeout)
	defer cancel()

	n, err := s.sessions.DeleteExpired(batchCtx, s.cfg.BatchSize)
	s.metrics.SessionsSwept.Add(float64(n))
	return n, err
}
