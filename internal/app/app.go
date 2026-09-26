// Package app is the composition root: it builds concrete dependencies and wires them into the processes.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"

	"github.com/onetodone/cinema-api/internal/config"
	"github.com/onetodone/cinema-api/internal/platform/pgpool"
	"github.com/onetodone/cinema-api/internal/platform/redisclient"
	"github.com/onetodone/cinema-api/internal/repository/postgres"
	redisrepo "github.com/onetodone/cinema-api/internal/repository/redis"
	"github.com/onetodone/cinema-api/internal/service/admin"
	"github.com/onetodone/cinema-api/internal/service/auth"
	"github.com/onetodone/cinema-api/internal/service/booking"
	"github.com/onetodone/cinema-api/internal/service/catalog"
	"github.com/onetodone/cinema-api/internal/transport/httpapi"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/handler"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/middleware"
)

const (
	appName          = "cinema-api"
	readinessTimeout = 2 * time.Second
	redisPingTimeout = 2 * time.Second
	// rateLimitWindow is the window of every *_RATE_LIMIT_PER_MIN setting.
	rateLimitWindow = time.Minute
)

// API is the HTTP API process with its external resources.
type API struct {
	cfg     config.Config
	logger  *slog.Logger
	db      *pgxpool.Pool
	redis   *redis.Client
	server  *server // the public API
	metrics *server // GET /metrics, on an internal address
}

// NewAPI connects to PostgreSQL and Redis and builds the HTTP servers.
// PostgreSQL must be reachable, because it is the source of truth. Redis may be down: the API starts anyway
// and keeps working correctly in fail-open mode.
func NewAPI(ctx context.Context, cfg config.Config, logger *slog.Logger) (*API, error) {
	// Checked before connecting, so a missing secret fails fast. Only the API needs it, which is why the
	// shared config does not require it.
	tokens, err := auth.NewTokens(cfg.Auth.JWTSecret, cfg.Auth.JWTTTL)
	if err != nil {
		return nil, fmt.Errorf("JWT_SECRET: %w", err)
	}

	db, err := pgpool.New(ctx, cfg.DB, appName)
	if err != nil {
		return nil, err
	}
	authSvc, err := auth.New(postgres.NewUsers(db), cfg.Auth.BcryptCost)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("BCRYPT_COST: %w", err)
	}
	sessions, err := auth.NewSessions(authSvc, postgres.NewSessions(db), tokens, auth.SessionConfig{
		IdleTTL:    cfg.Auth.RefreshTTL,
		MaxAge:     cfg.Auth.SessionMaxAge,
		Grace:      cfg.Auth.RefreshGrace,
		MaxPerUser: cfg.Auth.MaxSessionsPerUser,
	}, logger)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("session settings: %w", err)
	}
	if !cfg.Auth.CookieSecure {
		logger.WarnContext(ctx, "the refresh token cookie is not Secure (AUTH_COOKIE_SECURE=false); "+
			"browsers send it over plain HTTP, which is only acceptable in development")
	}

	rdb := newRedis(ctx, cfg.Redis, appName, logger)

	health := handler.NewHealth(logger, readinessTimeout, healthChecks(db, rdb)...)

	providers, err := newPaymentProviders(cfg.Payment)
	if err != nil {
		db.Close()
		return nil, err
	}
	logPaymentMethods(logger, providers)

	m, metricsHandler := newMetrics()
	for _, method := range providers.Methods() {
		m.InitPayments(method.ID)
	}
	store := redisrepo.New(rdb, cfg.Redis.KeyPrefix, m, logger)
	catalogCache := store.CatalogCache(cfg.Cache.SeatMapTTL, cfg.Cache.ScheduleTTL)

	catalogRepo := postgres.NewCatalog(db)
	catalogSvc := catalog.New(catalogRepo, cfg.Cinema.Location, catalog.WithCache(catalogCache))
	adminSvc := admin.New(catalogRepo, cfg.Cinema.Location, admin.WithScheduleCache(catalogCache))
	bookingSvc := newBookingService(db, cfg, providers, m, logger,
		booking.WithHoldGate(store.HoldGate()), booking.WithSeatMapCache(catalogCache))

	router := httpapi.NewRouter(httpapi.RouterDeps{
		Logger:  logger,
		Tokens:  tokens,
		Health:  health,
		Catalog: handler.NewCatalog(catalogSvc, cfg.Cinema.Currency, logger),
		Auth: handler.NewAuth(authSvc, sessions, handler.AuthConfig{
			Cookie:         handler.RefreshCookie{Path: cfg.Auth.CookiePath, Secure: cfg.Auth.CookieSecure},
			TrustedProxies: cfg.HTTP.TrustedProxies,
		}, m, logger),
		Bookings: handler.NewBookings(bookingSvc, cfg.Cinema.Currency, m, logger),
		Payments: handler.NewPayments(bookingSvc, providers, cfg.Cinema.Currency, m, logger),
		Admin:    handler.NewAdmin(adminSvc, cfg.Cinema.Currency, logger),

		Metrics:           m,
		Idempotency:       store.Idempotency(),
		BookingLimiter:    rateLimiter(store, "book", cfg.RateLimit.BookingPerMin),
		AuthIPLimiter:     rateLimiter(store, "auth-ip", cfg.RateLimit.AuthIPPerMin),
		LoginEmailLimiter: rateLimiter(store, "login-email", cfg.RateLimit.LoginEmailPerMin),
		RefreshLimiter:    rateLimiter(store, "refresh", cfg.RateLimit.RefreshPerMin),
		TrustedProxies:    cfg.HTTP.TrustedProxies,
	})

	return &API{
		cfg:     cfg,
		logger:  logger,
		db:      db,
		redis:   rdb,
		server:  newServer("http server", cfg.HTTP.Addr, router, cfg.HTTP, logger),
		metrics: newServer("metrics server", cfg.Metrics.Addr, metricsMux(metricsHandler, nil), cfg.HTTP, logger),
	}, nil
}

// healthChecks are the readiness checks of a process: PostgreSQL is critical, Redis is not, because every use of
// it fails open.
func healthChecks(db *pgxpool.Pool, rdb *redis.Client) []handler.Check {
	return []handler.Check{
		{Name: "postgres", Critical: true, Probe: db.Ping},
		{Name: "redis", Critical: false, Probe: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }},
	}
}

// newRedis returns a Redis client. Redis may be down at startup: the process starts anyway and works without it
// (fail open) until it is back.
func newRedis(ctx context.Context, cfg config.RedisConfig, clientName string, logger *slog.Logger) *redis.Client {
	redisclient.UseLogger(logger)
	rdb := redisclient.New(cfg, clientName)
	pingCtx, cancel := context.WithTimeout(ctx, redisPingTimeout)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		logger.WarnContext(ctx, "redis is unavailable at startup; continuing in fail-open mode",
			slog.String("addr", cfg.Addr), slog.Any("error", err))
	}
	return rdb
}

// rateLimiter returns a limiter of perMinute attempts per key, or nil, which turns the limit off, for 0.
func rateLimiter(store *redisrepo.Store, name string, perMinute int) middleware.RateLimiter {
	if perMinute == 0 {
		return nil
	}
	return store.RateLimiter(name, perMinute, rateLimitWindow)
}

// Run serves the API and its metrics until ctx is canceled, then stops accepting connections and waits for
// requests in flight for at most HTTP_SHUTDOWN_TIMEOUT. If one server fails, the other one stops too.
func (a *API) Run(ctx context.Context) error {
	if err := listen(ctx, a.server, a.metrics); err != nil {
		return err
	}
	g, ctx := errgroup.WithContext(ctx)
	for _, s := range []*server{a.server, a.metrics} {
		g.Go(func() error { return s.serve(ctx, a.logger, a.cfg.HTTP.ShutdownTimeout) })
	}
	return g.Wait()
}

// Close releases the database pool and the Redis client. Call it once, after Run has returned.
func (a *API) Close() {
	a.db.Close()
	if err := a.redis.Close(); err != nil {
		a.logger.Warn("closing redis client", slog.Any("error", err))
	}
}
