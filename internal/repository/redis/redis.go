// Package redis implements the Redis side of the application: the seat hold gate, the seat map and schedule
// caches, the idempotency records, and the rate limiters.
//
// Redis is an accelerator here, never the source of truth. Any operation may fail; it then returns an error, its
// caller carries on without Redis (fail open), and the failure is counted in cinema_redis_fail_open_total.
// PostgreSQL alone decides who owns a seat, so a Redis outage makes the system slower and less shielded from
// floods, but never incorrect.
//
// The adapters implement ports declared by the packages that use them: booking.HoldGate and
// booking.SeatMapCache, catalog.Cache, and the idempotency and rate limit stores of the HTTP middleware.
package redis

import (
	"context"
	"embed"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/onetodone/cinema-api/internal/platform/metrics"
)

// failureLogInterval is the least time between two warnings about failures of the same operation. The metric
// counts every failure; the log only has to tell an operator that Redis is in trouble, without writing a line
// for every request during an outage.
const failureLogInterval = 10 * time.Second

//go:embed lua/*.lua
var scripts embed.FS

// script loads an embedded Lua script. go-redis runs it with EVALSHA and falls back to EVAL when Redis does not
// know the script yet, for example after a restart.
func script(name string) *goredis.Script {
	src, err := scripts.ReadFile("lua/" + name)
	if err != nil {
		panic(err) // the file is embedded at build time; a missing one is a programming error
	}
	return goredis.NewScript(string(src))
}

// Store bundles what every adapter needs: the client, the key prefix, and the failure accounting.
type Store struct {
	rdb     goredis.UniversalClient
	prefix  string
	metrics *metrics.Metrics
	logger  *slog.Logger

	mu     sync.Mutex
	logged map[string]time.Time // when the last failure of each operation was logged
}

// New returns a Store that writes keys starting with prefix.
func New(rdb goredis.UniversalClient, prefix string, m *metrics.Metrics, logger *slog.Logger) *Store {
	return &Store{rdb: rdb, prefix: prefix, metrics: m, logger: logger, logged: map[string]time.Time{}}
}

// key joins the prefix and parts with colons.
func (s *Store) key(parts ...string) string {
	return s.prefix + ":" + strings.Join(parts, ":")
}

// failed records that the Redis operation op failed and that its caller carries on without Redis. A canceled
// context is not a failure of Redis: the caller went away.
func (s *Store) failed(ctx context.Context, op string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.metrics.RedisFailOpen.WithLabelValues(op).Inc()

	now := time.Now()
	s.mu.Lock()
	quiet := now.Sub(s.logged[op]) < failureLogInterval
	if !quiet {
		s.logged[op] = now
	}
	s.mu.Unlock()
	if !quiet {
		s.logger.WarnContext(ctx, "redis operation failed; carrying on without redis",
			slog.String("op", op), slog.Any("error", err))
	}
}
