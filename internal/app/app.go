// Package app is the composition root: it builds concrete dependencies and wires them into the HTTP server.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/onetodone/cinema-api/internal/config"
	"github.com/onetodone/cinema-api/internal/platform/pgpool"
	"github.com/onetodone/cinema-api/internal/platform/redisclient"
	"github.com/onetodone/cinema-api/internal/repository/postgres"
	"github.com/onetodone/cinema-api/internal/service/admin"
	"github.com/onetodone/cinema-api/internal/service/auth"
	"github.com/onetodone/cinema-api/internal/service/catalog"
	"github.com/onetodone/cinema-api/internal/transport/httpapi"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/handler"
)

const (
	appName          = "cinema-api"
	readinessTimeout = 2 * time.Second
	redisPingTimeout = 2 * time.Second
)

// API is the HTTP API process with its external resources.
type API struct {
	cfg    config.Config
	logger *slog.Logger
	db     *pgxpool.Pool
	redis  *redis.Client
	server *http.Server
}

// NewAPI connects to PostgreSQL and Redis and builds the HTTP server.
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

	rdb := redisclient.New(cfg.Redis, appName)
	pingCtx, cancel := context.WithTimeout(ctx, redisPingTimeout)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		logger.WarnContext(ctx, "redis is unavailable at startup; continuing in fail-open mode",
			slog.String("addr", cfg.Redis.Addr), slog.Any("error", err))
	}

	health := handler.NewHealth(logger, readinessTimeout,
		handler.Check{Name: "postgres", Critical: true, Probe: db.Ping},
		handler.Check{Name: "redis", Critical: false, Probe: func(ctx context.Context) error {
			return rdb.Ping(ctx).Err()
		}},
	)

	catalogRepo := postgres.NewCatalog(db)
	catalogSvc := catalog.New(catalogRepo, cfg.Cinema.Location)

	router := httpapi.NewRouter(httpapi.RouterDeps{
		Logger:  logger,
		Tokens:  tokens,
		Health:  health,
		Catalog: handler.NewCatalog(catalogSvc, cfg.Cinema.Currency, logger),
		Auth:    handler.NewAuth(authSvc, tokens, logger),
		Admin:   handler.NewAdmin(admin.New(catalogRepo), logger),
	})

	server := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           router,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	return &API{cfg: cfg, logger: logger, db: db, redis: rdb, server: server}, nil
}

// Run serves HTTP until ctx is canceled, then stops accepting connections and waits for in-flight requests
// for at most HTTP_SHUTDOWN_TIMEOUT.
func (a *API) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", a.server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", a.server.Addr, err)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- a.server.Serve(ln) }()
	a.logger.InfoContext(ctx, "http server started", slog.String("addr", ln.Addr().String()))

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	a.logger.Info("shutdown signal received; draining connections",
		slog.String("timeout", a.cfg.HTTP.ShutdownTimeout.String()))

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := a.server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}

	a.logger.Info("http server stopped")
	return nil
}

// Close releases the database pool and the Redis client. Call it once, after Run has returned.
func (a *API) Close() {
	a.db.Close()
	if err := a.redis.Close(); err != nil {
		a.logger.Warn("closing redis client", slog.Any("error", err))
	}
}
