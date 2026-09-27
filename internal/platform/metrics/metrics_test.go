package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandlerServesEveryMetricFromZero(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	m := New(reg)
	m.HoldGateRejections.Inc()
	m.RedisFailOpen.WithLabelValues(OpCacheGet).Add(2)
	m.InitPayments("local")
	m.ObserveRequest("GET /v1/movies", 200, 3*time.Millisecond)
	m.ObserveRequest("", 404, time.Millisecond)

	rec := httptest.NewRecorder()
	Handler(reg).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"cinema_hold_gate_rejections_total 1",
		`cinema_redis_fail_open_total{op="cache_get"} 2`,
		`cinema_redis_fail_open_total{op="hold_acquire"} 0`,
		`cinema_cache_requests_total{cache="seatmap",result="hit"} 0`,
		`cinema_rate_limit_rejections_total{limit="login_email"} 0`,
		`cinema_idempotency_requests_total{result="replayed"} 0`,
		`cinema_booking_attempts_total{result="seat_unavailable"} 0`,
		`cinema_db_tx_retries_total{sqlstate="40P01"} 0`,
		"cinema_bookings_expired_total 0",
		`cinema_payments_total{provider="local",result="pending"} 0`,
		`cinema_payments_reconciled_total{result="unsettled"} 0`,
		`cinema_auth_refresh_total{result="reuse_detected"} 0`,
		"cinema_sessions_swept_total 0",
		`cinema_session_revocations_total{reason="evicted"} 0`,
		`cinema_redis_fail_open_total{op="revocation_check"} 0`,
		`cinema_rate_limit_rejections_total{limit="refresh"} 0`,
		`cinema_http_request_duration_seconds_bucket{code="200",route="GET /v1/movies",le="0.005"} 1`,
		`cinema_http_request_duration_seconds_bucket{code="200",route="GET /v1/movies",le="0.0025"} 0`,
		`cinema_http_request_duration_seconds_count{code="404",route="unmatched"} 1`,
		"go_goroutines",
		"process_cpu_seconds_total",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics output lacks %q", want)
		}
	}
}
