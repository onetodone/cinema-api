package app

import (
	"context"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/onetodone/cinema-api/internal/config"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/platform/pgpool"
	redisrepo "github.com/onetodone/cinema-api/internal/repository/redis"
	"github.com/onetodone/cinema-api/internal/service/booking"
	"github.com/onetodone/cinema-api/internal/worker"
)

const workerName = "cinema-worker"

// Worker is the background process: it expires unpaid bookings whose hold has run out, and settles payments
// that the API left in flight. Any number of replicas may run next to each other and next to the API.
type Worker struct {
	cfg        config.Config
	logger     *slog.Logger
	db         *pgxpool.Pool
	redis      *redis.Client
	expirer    *worker.Expirer
	reconciler *worker.Reconciler
}

// NewWorker connects to PostgreSQL and Redis and builds the background jobs. Redis may be down: the jobs only use
// it to release the hold gate claims of the bookings they expire and to invalidate the cached seat maps of the
// showtimes they change. Both expire by themselves otherwise.
func NewWorker(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Worker, error) {
	db, err := pgpool.New(ctx, cfg.DB, workerName)
	if err != nil {
		return nil, err
	}

	providers, err := newPaymentProviders(cfg.Payment)
	if err != nil {
		db.Close()
		return nil, err
	}
	rdb := newRedis(ctx, cfg.Redis, workerName, logger)
	// The worker serves no metrics yet (Sprint 7), so they go to a registry of their own.
	store := redisrepo.New(rdb, cfg.Redis.KeyPrefix, metrics.New(prometheus.NewRegistry()), logger)
	bookingSvc := newBookingService(db, cfg, providers, logger,
		booking.WithHoldGate(store.HoldGate()),
		booking.WithSeatMapCache(store.CatalogCache(cfg.Cache.SeatMapTTL, cfg.Cache.ScheduleTTL)))
	expirer := worker.NewExpirer(bookingSvc,
		worker.ExpirerConfig{Interval: cfg.Expirer.Interval, BatchSize: cfg.Expirer.BatchSize}, logger)
	reconciler := worker.NewReconciler(bookingSvc,
		worker.ReconcilerConfig{Interval: cfg.Reconciler.Interval, BatchSize: worker.DefaultReconcileBatchSize}, logger)

	return &Worker{cfg: cfg, logger: logger, db: db, redis: rdb, expirer: expirer, reconciler: reconciler}, nil
}

// Run runs the jobs side by side until ctx is canceled. Each job finishes what it is working on before Run
// returns.
func (w *Worker) Run(ctx context.Context) error {
	w.logger.InfoContext(ctx, "worker started",
		slog.String("expirer_interval", w.cfg.Expirer.Interval.String()),
		slog.Int("expirer_batch_size", w.cfg.Expirer.BatchSize),
		slog.String("reconciler_interval", w.cfg.Reconciler.Interval.String()),
		slog.String("payment_grace", w.cfg.Payment.Grace.String()))

	var jobs sync.WaitGroup
	jobs.Go(func() { w.expirer.Run(ctx) })
	jobs.Go(func() { w.reconciler.Run(ctx) })
	jobs.Wait()

	w.logger.Info("worker stopped")
	return nil
}

// Close releases the database pool and the Redis client. Call it once, after Run has returned.
func (w *Worker) Close() {
	w.db.Close()
	if err := w.redis.Close(); err != nil {
		w.logger.Warn("closing redis client", slog.Any("error", err))
	}
}
