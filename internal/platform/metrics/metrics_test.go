package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesEveryMetricFromZero(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	m := New(reg)
	m.HoldGateRejections.Inc()
	m.RedisFailOpen.WithLabelValues(OpCacheGet).Add(2)

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
		"go_goroutines",
		"process_cpu_seconds_total",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics output lacks %q", want)
		}
	}
}
