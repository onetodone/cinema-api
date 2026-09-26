package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/service/booking"
)

// batchTimeout bounds one expiry transaction. A batch takes milliseconds; the timeout only matters when the
// database hangs, and it caps how long shutdown waits for the batch in flight.
const batchTimeout = 30 * time.Second

// BookingExpirer expires due bookings in batches. It is implemented by booking.Service.
type BookingExpirer interface {
	ExpireBatch(ctx context.Context, limit int) (booking.ExpiredBatch, error)
}

// ExpirerConfig configures an Expirer.
type ExpirerConfig struct {
	Interval  time.Duration // average pause between two sweeps
	BatchSize int           // most bookings expired per transaction
}

// Expirer makes the seats of unpaid bookings available again once their hold has run out.
//
// Correctness does not depend on it: payment refuses a booking whose hold has run out, whether or not the
// expirer has been there yet, and every decision uses the database clock. The expirer returns the seats to
// sale and frees the owner's slot for another booking of the showtime. Any number of expirers may run at
// once, in one process or in many; the database hands every due booking to exactly one of them.
type Expirer struct {
	bookings BookingExpirer
	cfg      ExpirerConfig
	metrics  *metrics.Metrics
	logger   *slog.Logger
}

// NewExpirer returns an Expirer that counts the bookings it expires in m. It panics if the interval or the batch
// size is not positive.
func NewExpirer(bookings BookingExpirer, cfg ExpirerConfig, m *metrics.Metrics, logger *slog.Logger) *Expirer {
	if cfg.Interval <= 0 || cfg.BatchSize <= 0 {
		panic(fmt.Sprintf("worker: expirer interval and batch size must be positive, got %s and %d",
			cfg.Interval, cfg.BatchSize))
	}
	return &Expirer{bookings: bookings, cfg: cfg, metrics: m, logger: logger.With(slog.String("job", "expirer"))}
}

// Run sweeps at once and then about every interval until ctx is canceled. A failed batch is logged and
// retried by the next sweep. Once ctx is canceled, Run lets the batch in flight finish, starts no other one,
// and returns.
func (e *Expirer) Run(ctx context.Context) {
	every(ctx, e.cfg.Interval, e.sweep)
}

// sweep expires due bookings batch by batch until a batch comes back short, which means that nothing else was
// due, or a batch fails, or ctx is canceled.
func (e *Expirer) sweep(ctx context.Context) {
	for ctx.Err() == nil {
		n, err := e.expireBatch(ctx)
		if err != nil || n < e.cfg.BatchSize {
			return
		}
	}
}

func (e *Expirer) expireBatch(ctx context.Context) (int, error) {
	// Shutdown does not cut a batch off. It is one short transaction, and finishing it is cheaper than rolling
	// it back for the next sweep to redo.
	batchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), batchTimeout)
	defer cancel()

	start := time.Now()
	batch, err := e.bookings.ExpireBatch(batchCtx, e.cfg.BatchSize)
	switch {
	case errors.Is(err, domain.ErrBusy):
		e.logger.WarnContext(ctx, "expiry batch gave up waiting for a lock; the next sweep retries it",
			slog.Any("error", err))
	case err != nil:
		e.logger.ErrorContext(ctx, "expiry batch failed; the next sweep retries it", slog.Any("error", err))
	case len(batch.BookingIDs) > 0:
		e.metrics.BookingsExpired.Add(float64(len(batch.BookingIDs)))
		e.logger.InfoContext(ctx, "bookings expired",
			slog.Int("bookings", len(batch.BookingIDs)),
			slog.Int64("seats", batch.Seats),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()))
	}
	return len(batch.BookingIDs), err
}
