// Package metrics defines the application's Prometheus metrics and serves them.
//
// Every metric the application exports is declared here, so this file is the list of them. Packages that record
// a metric receive the *Metrics of their process; tests give each case its own registry. The API and the worker
// export the same set: a counter that a process never records stays at 0, and queries tell the processes apart by
// their scrape job.
package metrics

import (
	"net/http"
	"strconv"
	"time"

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

// Outcomes of booking requests, the result label of BookingAttempts.
const (
	BookingCreated             = "created"
	BookingSeatUnavailable     = "seat_unavailable"      // 409 SEAT_UNAVAILABLE, from the hold gate or PostgreSQL
	BookingBusy                = "busy"                  // 409 SEAT_BUSY or BOOKING_BUSY: a lock was not granted in time
	BookingActiveBookingExists = "active_booking_exists" // 409 ACTIVE_BOOKING_EXISTS
	BookingRejected            = "rejected"              // any other client error, such as invalid input
	BookingFailed              = "failed"                // a server error
)

// SQLSTATEs of the transactions that PostgreSQL aborted and the unit of work ran again, the sqlstate label of
// TxRetries.
const (
	SQLStateDeadlock             = "40P01"
	SQLStateSerializationFailure = "40001"
)

// Outcomes of payments, the result label of Payments.
const (
	PaymentSucceeded           = "succeeded"
	PaymentDeclined            = "declined"
	PaymentProviderUnavailable = "provider_unavailable" // the provider refused the request; nothing was charged
	PaymentPending             = "pending"              // outcome unknown for now: the worker settles it
	PaymentRefunded            = "refunded"             // charged after the payment was given up, and refunded
)

// Outcomes of stuck payments that the worker looked at, the result label of PaymentsReconciled. A payment that
// someone else settled first is not counted.
const (
	ReconciledPaid      = "paid"
	ReconciledFailed    = "failed"    // settled as failed or refunded; the booking is pending again, or expired
	ReconciledUnsettled = "unsettled" // the provider or the database failed; the next pass retries it
)

// RouteUnmatched is the route label of requests that matched no route.
const RouteUnmatched = "unmatched"

// httpDurationBuckets cover everything from a cache hit (about a millisecond) to a request that waited for a lock
// or a payment provider (seconds), in seconds.
var httpDurationBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// Metrics holds the application's metrics.
type Metrics struct {
	// HTTPRequestDuration observes how long the API took to answer, by route pattern (which starts with the
	// method, such as "POST /v1/bookings") and status code. The request path is never a label, so a client
	// cannot create new series at will.
	HTTPRequestDuration *prometheus.HistogramVec
	// BookingAttempts counts booking requests that reached the booking handler, by result. Requests turned away
	// earlier (rate limit, idempotent replay, missing token) are not attempts.
	BookingAttempts *prometheus.CounterVec
	// TxRetries counts transactions that PostgreSQL aborted with a deadlock or a serialization failure, and that
	// the unit of work ran again, by SQLSTATE. The lock order should keep it at 0.
	TxRetries *prometheus.CounterVec
	// BookingsExpired counts unpaid bookings whose hold ran out and whose seats the worker made available again.
	BookingsExpired prometheus.Counter
	// Payments counts payments that the API started, by provider and result.
	Payments *prometheus.CounterVec
	// PaymentsReconciled counts stuck payments the worker settled or failed to settle, by result.
	PaymentsReconciled *prometheus.CounterVec

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
		HTTPRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "http_request_duration_seconds",
			Help:    "Time the API took to answer requests, by route and status code.",
			Buckets: httpDurationBuckets,
		}, []string{"route", "code"}),
		BookingAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "booking_attempts_total",
			Help: "Booking requests by result.",
		}, []string{"result"}),
		TxRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "db_tx_retries_total",
			Help: "Transactions aborted by PostgreSQL and run again, by SQLSTATE.",
		}, []string{"sqlstate"}),
		BookingsExpired: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "bookings_expired_total",
			Help: "Unpaid bookings expired by the worker; their seats are available again.",
		}),
		Payments: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "payments_total",
			Help: "Payments started through the API, by provider and result.",
		}, []string{"provider", "result"}),
		PaymentsReconciled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "payments_reconciled_total",
			Help: "Stuck payments looked at by the worker, by result.",
		}, []string{"result"}),
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
	reg.MustRegister(
		m.HTTPRequestDuration, m.BookingAttempts, m.TxRetries, m.BookingsExpired, m.Payments, m.PaymentsReconciled,
		m.HoldGateRejections, m.RedisFailOpen, m.CacheRequests, m.RateLimitRejections, m.IdempotencyRequests,
	)

	for _, result := range []string{
		BookingCreated, BookingSeatUnavailable, BookingBusy, BookingActiveBookingExists, BookingRejected, BookingFailed,
	} {
		m.BookingAttempts.WithLabelValues(result)
	}
	for _, code := range []string{SQLStateDeadlock, SQLStateSerializationFailure} {
		m.TxRetries.WithLabelValues(code)
	}
	for _, result := range []string{ReconciledPaid, ReconciledFailed, ReconciledUnsettled} {
		m.PaymentsReconciled.WithLabelValues(result)
	}

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

// InitPayments starts the payment counters of the given providers at zero. The providers are known only once the
// process has read its configuration.
func (m *Metrics) InitPayments(providers ...string) {
	for _, p := range providers {
		for _, result := range []string{
			PaymentSucceeded, PaymentDeclined, PaymentProviderUnavailable, PaymentPending, PaymentRefunded,
		} {
			m.Payments.WithLabelValues(p, result)
		}
	}
}

// ObserveRequest records how long a request took. route is the pattern of the route that served it; empty means
// that no route matched.
func (m *Metrics) ObserveRequest(route string, code int, d time.Duration) {
	if route == "" {
		route = RouteUnmatched
	}
	m.HTTPRequestDuration.WithLabelValues(route, strconv.Itoa(code)).Observe(d.Seconds())
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
