package middleware

import (
	"net/http"
	"time"

	"github.com/onetodone/cinema-api/internal/platform/metrics"
)

// Instrument observes how long every request takes in cinema_http_request_duration_seconds, by the pattern of the
// route that served it and the status code. Like AccessLog, it must wrap the ServeMux without a middleware in
// between that replaces the *http.Request, because the mux records the pattern on the request it receives.
func Instrument(m *metrics.Metrics) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			m.ObserveRequest(r.Pattern, rec.status, time.Since(start))
		})
	}
}
