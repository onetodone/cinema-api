//go:build integration

package integration

import (
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	goredis "github.com/redis/go-redis/v9"

	"github.com/onetodone/cinema-api/internal/config"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/platform/redisclient"
	redisrepo "github.com/onetodone/cinema-api/internal/repository/redis"
)

// deadRedisAddr refuses connections: nothing listens on port 1.
const deadRedisAddr = "127.0.0.1:1"

var prefixCounter atomic.Int64

// redisEnv is a Redis store with a key prefix of its own, and the metrics and logs it records.
type redisEnv struct {
	store   *redisrepo.Store
	client  *goredis.Client
	metrics *metrics.Metrics
	logs    *logRecorder
	prefix  string
}

// newRedisEnv connects to the test Redis.
func newRedisEnv(t *testing.T) *redisEnv {
	t.Helper()
	return newRedisEnvAt(t, redisAddr)
}

// newDeadRedisEnv points at an address where no Redis listens, like an outage.
func newDeadRedisEnv(t *testing.T) *redisEnv {
	t.Helper()
	return newRedisEnvAt(t, deadRedisAddr)
}

func newRedisEnvAt(t *testing.T, addr string) *redisEnv {
	t.Helper()
	// The client settings of .env.example: fail fast.
	client := redisclient.New(config.RedisConfig{
		Addr: addr, DialTimeout: time.Second, ReadTimeout: 500 * time.Millisecond, WriteTimeout: 500 * time.Millisecond,
		DialAttempts: 1, MaxRetries: 1,
	}, "cinema-test")
	t.Cleanup(func() { _ = client.Close() })

	env := &redisEnv{
		client:  client,
		metrics: metrics.New(prometheus.NewRegistry()),
		logs:    &logRecorder{},
		prefix:  fmt.Sprintf("test%d", prefixCounter.Add(1)),
	}
	env.store = redisrepo.New(client, env.prefix, env.metrics, slog.New(env.logs))
	return env
}

// failedOpen returns how often op failed and was skipped.
func (e *redisEnv) failedOpen(op string) float64 {
	return testutil.ToFloat64(e.metrics.RedisFailOpen.WithLabelValues(op))
}

// gateRejections returns how many booking requests the hold gate turned away.
func (e *redisEnv) gateRejections() float64 {
	return testutil.ToFloat64(e.metrics.HoldGateRejections)
}

// cacheRequests returns the lookups of cache that ended with result.
func (e *redisEnv) cacheRequests(cache, result string) float64 {
	return testutil.ToFloat64(e.metrics.CacheRequests.WithLabelValues(cache, result))
}

// testCount returns the value of a counter.
func testCount(c prometheus.Counter) float64 {
	return testutil.ToFloat64(c)
}
