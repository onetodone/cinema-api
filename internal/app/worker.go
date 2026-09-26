package app

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onetodone/cinema-api/internal/config"
	"github.com/onetodone/cinema-api/internal/platform/pgpool"
	"github.com/onetodone/cinema-api/internal/repository/postgres"
	"github.com/onetodone/cinema-api/internal/service/booking"
	"github.com/onetodone/cinema-api/internal/worker"
)

const workerName = "cinema-worker"

// Worker is the background process: it expires unpaid bookings whose hold has run out. Any number of
// replicas may run next to each other and next to the API.
type Worker struct {
	cfg     config.Config
	logger  *slog.Logger
	db      *pgxpool.Pool
	expirer *worker.Expirer
}

// NewWorker connects to PostgreSQL and builds the background jobs.
func NewWorker(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Worker, error) {
	db, err := pgpool.New(ctx, cfg.DB, workerName)
	if err != nil {
		return nil, err
	}

	bookingSvc := booking.New(
		postgres.NewUnitOfWork(db, cfg.DB.LockTimeout, logger),
		postgres.NewBookings(db),
		cfg.Cinema.Location,
		booking.Config{HoldTTL: cfg.Booking.HoldTTL, MaxSeats: cfg.Booking.MaxSeats},
	)
	expirer := worker.NewExpirer(bookingSvc,
		worker.ExpirerConfig{Interval: cfg.Expirer.Interval, BatchSize: cfg.Expirer.BatchSize}, logger)

	return &Worker{cfg: cfg, logger: logger, db: db, expirer: expirer}, nil
}

// Run runs the jobs until ctx is canceled. Each job finishes the batch it is working on before Run returns.
func (w *Worker) Run(ctx context.Context) error {
	w.logger.InfoContext(ctx, "worker started",
		slog.String("expirer_interval", w.cfg.Expirer.Interval.String()),
		slog.Int("expirer_batch_size", w.cfg.Expirer.BatchSize))

	w.expirer.Run(ctx)

	w.logger.Info("worker stopped")
	return nil
}

// Close releases the database pool. Call it once, after Run has returned.
func (w *Worker) Close() {
	w.db.Close()
}
