// Package metrics defines the application's Prometheus metrics and serves them.
//
// Every metric the application exports is declared here, so this file is the list of them. Packages that record
// a metric receive the *Metrics of their process; tests give each case its own registry.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "cinema"

// Redis operations that fail open, the op label of RedisFailOpen.
const (
	OpHoldAcquire     = "hold_acquire"
	OpHoldExtend      = "hold_extend"
	OpHoldRelease     = "hold_release"
	OpCacheGet        = "cache_get"
	OpCacheSet        = "cache_set"
	OpCacheInvalidate = "cache_invalidate"
	OpIdempotency     = "idempotency"
	OpRateLimit       = "rate_limit"
)

// Caches, the cache label of CacheRequests.
const (
	CacheSeatMap  = "seatmap"
	CacheSchedule = "schedule"
)

// Cache lookup results, the result label of CacheRequests. A lookup that failed counts as an error; the caller
// then reads from PostgreSQL, like on a miss.
const (
	CacheHit   = "hit"
	CacheMiss  = "miss"
	CacheError = "error"
)

// Rate limits, the limit label of RateLimitRejections.
const (
	LimitBooking    = "booking"
	LimitAuthIP     = "auth_ip"
	LimitLoginEmail = "login_email"
)

// Outcomes of requests that carry an Idempotency-Key, the result label of IdempotencyRequests.
const (
	IdempotencyNew        = "new"         // first request with the key: it runs
	IdempotencyReplayed   = "replayed"    // the stored response is sent again
	IdempotencyInProgress = "in_progress" // 409: the first request is still running
	IdempotencyKeyReused  = "key_reused"  // 422: the key was used for a different request
)

// Metrics holds the application's metrics.
type Metrics struct {
	// HoldGateRejections counts booking requests that the Redis hold gate turned away because another booking
	// holds or claims one of their seats. They never reached PostgreSQL.
	HoldGateRejections prometheus.Counter
	// RedisFailOpen counts Redis operations that failed, by op. The request carried on without Redis: slower
	// or less protected, but still correct, because PostgreSQL decides.
	RedisFailOpen *prometheus.CounterVec
	// CacheRequests counts cache lookups, by cache and result.
	CacheRequests *prometheus.CounterVec
	// RateLimitRejections counts requests answered with 429, by limit.
	RateLimitRejections *prometheus.CounterVec
	// IdempotencyRequests counts requests that carry an Idempotency-Key, by result.
	IdempotencyRequests *prometheus.CounterVec
}

// New creates the metrics and registers them with reg. Every known label combination starts at zero, so that
// rates and ratios exist before the first event.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		HoldGateRejections: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "hold_gate_rejections_total",
			Help: "Booking requests rejected by the Redis hold gate before reaching PostgreSQL.",
		}),
		RedisFailOpen: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "redis_fail_open_total",
			Help: "Redis operations that failed; the request carried on without Redis.",
		}, []string{"op"}),
		CacheRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "cache_requests_total",
			Help: "Cache lookups by cache and result.",
		}, []string{"cache", "result"}),
		RateLimitRejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "rate_limit_rejections_total",
			Help: "Requests rejected with 429 by a rate limit.",
		}, []string{"limit"}),
		IdempotencyRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "idempotency_requests_total",
			Help: "Requests with an Idempotency-Key by result.",
		}, []string{"result"}),
	}
	reg.MustRegister(m.HoldGateRejections, m.RedisFailOpen, m.CacheRequests, m.RateLimitRejections, m.IdempotencyRequests)

	for _, op := range []string{
		OpHoldAcquire, OpHoldExtend, OpHoldRelease, OpCacheGet, OpCacheSet, OpCacheInvalidate, OpIdempotency, OpRateLimit,
	} {
		m.RedisFailOpen.WithLabelValues(op)
	}
	for _, cache := range []string{CacheSeatMap, CacheSchedule} {
		for _, result := range []string{CacheHit, CacheMiss, CacheError} {
			m.CacheRequests.WithLabelValues(cache, result)
		}
	}
	for _, limit := range []string{LimitBooking, LimitAuthIP, LimitLoginEmail} {
		m.RateLimitRejections.WithLabelValues(limit)
	}
	for _, result := range []string{IdempotencyNew, IdempotencyReplayed, IdempotencyInProgress, IdempotencyKeyReused} {
		m.IdempotencyRequests.WithLabelValues(result)
	}
	return m
}

// NewRegistry returns a registry with the Go runtime and process collectors, for a process that serves its
// metrics.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return reg
}

// Handler serves the metrics of reg in the Prometheus exposition format.
func Handler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})
}
