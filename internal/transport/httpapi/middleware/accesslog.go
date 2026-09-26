package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// probePaths are logged at debug level so that load-balancer health checks and metric scrapes do not flood the
// logs.
var probePaths = map[string]bool{"/healthz": true, "/readyz": true, "/metrics": true}

// AccessLog writes one structured log line per request with its route, status, size, and latency.
// It must run inside RequestID so that the line carries the request ID.
func AccessLog(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			level := slog.LevelInfo
			switch {
			case rec.status >= http.StatusInternalServerError:
				level = slog.LevelError
			case probePaths[r.URL.Path]:
				level = slog.LevelDebug
			}

			logger.LogAttrs(r.Context(), level, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("route", r.Pattern), // set by ServeMux on this same *Request
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.bytes),
				slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
				slog.String("remote_addr", r.RemoteAddr),
			)
		})
	}
}

// statusRecorder captures the status code and body size written by the wrapped handler.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	// 1xx responses are informational; the final status comes later.
	if !r.wroteHeader && code >= http.StatusOK {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer (Flush, deadlines, hijacking).
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
