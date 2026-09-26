package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onetodone/cinema-api/internal/service/booking"
)

// Transaction retry policy. PostgreSQL aborts a deadlock victim with 40P01, and a transaction that cannot be
// serialized with 40001. Either way nothing of the transaction is left, so running it again from the start is
// safe. The lock order of the booking use cases should make deadlocks impossible; the retry is a safety net,
// and every retry is logged at warn level so it does not go unnoticed.
const (
	maxTxAttempts  = 4 // the first attempt plus 3 retries
	retryBaseDelay = 10 * time.Millisecond
)

// UnitOfWork runs the booking use cases in transactions. It implements booking.UnitOfWork.
type UnitOfWork struct {
	pool        *pgxpool.Pool
	lockTimeout string // a PostgreSQL duration, such as "3000ms"
	logger      *slog.Logger
}

// NewUnitOfWork returns a UnitOfWork on pool. A statement that waits longer than lockTimeout for a row lock
// fails; the repositories map that to a "busy" error the client may retry.
func NewUnitOfWork(pool *pgxpool.Pool, lockTimeout time.Duration, logger *slog.Logger) *UnitOfWork {
	// PostgreSQL counts lock_timeout in whole milliseconds, and 0 would disable it.
	ms := max(lockTimeout.Milliseconds(), 1)
	return &UnitOfWork{pool: pool, lockTimeout: fmt.Sprintf("%dms", ms), logger: logger}
}

// Do runs fn in a READ COMMITTED transaction with the lock timeout set. It commits when fn returns nil, and
// rolls back otherwise. A deadlock or serialization failure runs fn again, up to maxTxAttempts times in all.
func (u *UnitOfWork) Do(ctx context.Context, fn func(ctx context.Context, r booking.TxRepos) error) error {
	return retry(ctx, u.logger, func() error {
		return pgx.BeginTxFunc(ctx, u.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
			// SET LOCAL takes no bind parameters; set_config with is_local = true is its parameterized form.
			if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, u.lockTimeout); err != nil {
				return fmt.Errorf("set lock timeout: %w", err)
			}
			return fn(ctx, txRepos{q: tx})
		})
	})
}

// retry calls attempt until it succeeds, fails with an error that a retry cannot fix, or has failed
// maxTxAttempts times. It returns the error of the last attempt.
func retry(ctx context.Context, logger *slog.Logger, attempt func() error) error {
	for n := 1; ; n++ {
		err := attempt()
		code := pgErrorCode(err)
		if (code != sqlstateDeadlockDetected && code != sqlstateSerializationFailure) || n == maxTxAttempts ||
			ctx.Err() != nil {
			return err
		}

		// Full jitter: two transactions that collided should not collide again on their retries.
		delay := rand.N(retryBaseDelay << (n - 1)) //nolint:gosec // G404: jitter needs no cryptographic randomness
		logger.WarnContext(ctx, "transaction aborted by the database; retrying",
			slog.String("sqlstate", code), slog.Int("attempt", n), slog.Duration("delay", delay), slog.Any("error", err))

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}

// txRepos hands out the repositories of one transaction. It implements booking.TxRepos.
type txRepos struct {
	q querier
}

func (r txRepos) Showtimes() booking.ShowtimeRepo { return showtimeStore(r) }
func (r txRepos) Seats() booking.SeatRepo         { return seatStore(r) }
func (r txRepos) Bookings() booking.Repo          { return bookingStore(r) }
func (r txRepos) Payments() booking.PaymentRepo   { return paymentStore(r) }
