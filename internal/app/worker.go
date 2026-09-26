package app

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"

	"github.com/onetodone/cinema-api/internal/config"
	"github.com/onetodone/cinema-api/internal/platform/pgpool"
	redisrepo "github.com/onetodone/cinema-api/internal/repository/redis"
	"github.com/onetodone/cinema-api/internal/service/booking"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/handler"
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
	server     *server // GET /metrics, /healthz, and /readyz
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
	m, metricsHandler := newMetrics()
	store := redisrepo.New(rdb, cfg.Redis.KeyPrefix, m, logger)
	bookingSvc := newBookingService(db, cfg, providers, m, logger,
		booking.WithHoldGate(store.HoldGate()),
		booking.WithSeatMapCache(store.CatalogCache(cfg.Cache.SeatMapTTL, cfg.Cache.ScheduleTTL)))
	expirer := worker.NewExpirer(bookingSvc,
		worker.ExpirerConfig{Interval: cfg.Expirer.Interval, BatchSize: cfg.Expirer.BatchSize}, m, logger)
	reconciler := worker.NewReconciler(bookingSvc,
		worker.ReconcilerConfig{Interval: cfg.Reconciler.Interval, BatchSize: worker.DefaultReconcileBatchSize}, m, logger)

	health := handler.NewHealth(logger, readinessTimeout, healthChecks(db, rdb)...)
	mux := metricsMux(metricsHandler, map[string]http.HandlerFunc{
		"GET /healthz": health.Live,
		"GET /readyz":  health.Ready,
	})

	return &Worker{
		cfg:        cfg,
		logger:     logger,
		db:         db,
		redis:      rdb,
		expirer:    expirer,
		reconciler: reconciler,
		server:     newServer("metrics server", cfg.Metrics.WorkerAddr, mux, cfg.HTTP, logger),
	}, nil
}

// Run runs the jobs side by side, and serves the metrics and health probes, until ctx is canceled. Each job
// finishes what it is working on before Run returns. If the server fails, the jobs stop too, and Run returns the
// error, so that an orchestrator restarts a worker it can no longer watch.
func (w *Worker) Run(ctx context.Context) error {
	if err := listen(ctx, w.server); err != nil {
		return err
	}
	w.logger.InfoContext(ctx, "worker started",
		slog.String("expirer_interval", w.cfg.Expirer.Interval.String()),
		slog.Int("expirer_batch_size", w.cfg.Expirer.BatchSize),
		slog.String("reconciler_interval", w.cfg.Reconciler.Interval.String()),
		slog.String("payment_grace", w.cfg.Payment.Grace.String()))

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { w.expirer.Run(ctx); return nil })
	g.Go(func() error { w.reconciler.Run(ctx); return nil })
	g.Go(func() error { return w.server.serve(ctx, w.logger, w.cfg.HTTP.ShutdownTimeout) })
	err := g.Wait()

	w.logger.Info("worker stopped")
	return err
}

// Close releases the database pool and the Redis client. Call it once, after Run has returned.
func (w *Worker) Close() {
	w.db.Close()
	if err := w.redis.Close(); err != nil {
		w.logger.Warn("closing redis client", slog.Any("error", err))
	}
}
