// Package redisclient creates the Redis client.
package redisclient

import (
	"context"
	"fmt"
	"log/slog"
	"crypto/tls"

	"github.com/redis/go-redis/v9"

	"github.com/onetodone/cinema-api/internal/config"
)

// New returns a Redis client for cfg. It does not connect eagerly: Redis is an optional accelerator,
// so callers decide how to react when it is unreachable (the API keeps serving in fail-open mode).
func New(cfg config.RedisConfig, clientName string) *redis.Client {
	opts := &redis.Options{
		Addr:          cfg.Addr,
		Password:      cfg.Password,
		DB:            cfg.DB,
		DialTimeout:   cfg.DialTimeout,
		DialerRetries: cfg.DialAttempts, // go-redis counts total attempts here, not retries
		ReadTimeout:   cfg.ReadTimeout,
		WriteTimeout:  cfg.WriteTimeout,
		MaxRetries:    cfg.MaxRetries,
		ClientName:    clientName,
	}

	if cfg.TLSEnabled {
		opts.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	return redis.NewClient(opts)
}

// UseLogger sends the internal log lines of go-redis to logger at debug level. Without it, go-redis writes an
// unstructured line to stderr for every failed dial, which during an outage is one per request. The Redis
// adapters report failures themselves, counted in a metric and logged at most once per interval.
//
// go-redis has one logger per process, so call UseLogger once at startup.
func UseLogger(logger *slog.Logger) {
	redis.SetLogger(slogPrinter{logger: logger.With(slog.String("component", "go-redis"))})
}

type slogPrinter struct {
	logger *slog.Logger
}

// Printf logs at debug level. slog accepts the nil context that go-redis passes in some places.
func (p slogPrinter) Printf(ctx context.Context, format string, v ...any) {
	p.logger.DebugContext(ctx, fmt.Sprintf(format, v...))
}
