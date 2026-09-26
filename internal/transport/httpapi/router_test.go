package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/onetodone/cinema-api/internal/transport/httpapi/handler"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/middleware"
)

func newTestRouter() http.Handler {
	logger := slog.New(slog.DiscardHandler)
	return NewRouter(RouterDeps{
		Logger: logger,
		Health: handler.NewHealth(logger, time.Second,
			handler.Check{Name: "postgres", Critical: true, Probe: func(context.Context) error { return nil }},
		),
	})
}

func TestRouterServesProbes(t *testing.T) {
	t.Parallel()

	router := newTestRouter()
	for _, path := range []string{"/healthz", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))

			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
			if rec.Header().Get(middleware.RequestIDHeader) == "" {
				t.Error("response has no request ID header")
			}
		})
	}
}

func TestRouterRejectsWrongMethodAndUnknownPath(t *testing.T) {
	t.Parallel()

	router := newTestRouter()
	tests := []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodPost, path: "/healthz", want: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/nope", want: http.StatusNotFound},
	}

	for _, tt := range tests {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil))
		if rec.Code != tt.want {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, rec.Code, tt.want)
		}
	}
}
