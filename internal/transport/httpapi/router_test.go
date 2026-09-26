package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/service/catalog"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/handler"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/middleware"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

// emptyCatalog answers every catalog query with empty results.
type emptyCatalog struct{}

func (emptyCatalog) ListMovies(context.Context, int64, int) (catalog.MoviePage, error) {
	return catalog.MoviePage{}, nil
}

func (emptyCatalog) GetMovie(context.Context, int64) (catalog.MovieDetails, error) {
	return catalog.MovieDetails{}, nil
}

func (emptyCatalog) Schedule(context.Context, catalog.ScheduleQuery) (catalog.Schedule, error) {
	return catalog.Schedule{}, nil
}

func (emptyCatalog) GetShowtime(context.Context, int64) (domain.Showtime, error) {
	return domain.Showtime{}, nil
}

func (emptyCatalog) SeatMap(context.Context, int64) (catalog.SeatMap, error) {
	return catalog.SeatMap{}, nil
}

func newTestRouter() http.Handler {
	logger := slog.New(slog.DiscardHandler)
	return NewRouter(RouterDeps{
		Logger: logger,
		Health: handler.NewHealth(logger, time.Second,
			handler.Check{Name: "postgres", Critical: true, Probe: func(context.Context) error { return nil }},
		),
		Catalog: handler.NewCatalog(emptyCatalog{}, "USD", logger),
	})
}

func serve(t *testing.T, router http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, target, nil))
	return rec
}

func TestRouterServesRegisteredRoutes(t *testing.T) {
	t.Parallel()

	router := newTestRouter()
	for _, path := range []string{
		"/healthz", "/readyz",
		"/v1/movies", "/v1/movies/1",
		"/v1/showtimes", "/v1/showtimes/1", "/v1/showtimes/1/seats",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			rec := serve(t, router, http.MethodGet, path)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
			}
			if rec.Header().Get(middleware.RequestIDHeader) == "" {
				t.Error("response has no request ID header")
			}
		})
	}
}

func TestRouterUnmatchedRequestsGetProblemDetails(t *testing.T) {
	t.Parallel()

	router := newTestRouter()
	tests := []struct {
		method string
		path   string
		status int
		code   string
		allow  string
	}{
		{method: http.MethodGet, path: "/nope", status: http.StatusNotFound, code: problem.CodeNotFound},
		{method: http.MethodGet, path: "/v1/movies/1/extra", status: http.StatusNotFound, code: problem.CodeNotFound},
		{
			method: http.MethodPost, path: "/v1/movies",
			status: http.StatusMethodNotAllowed, code: problem.CodeMethodNotAllowed, allow: "GET, HEAD",
		},
		{
			method: http.MethodDelete, path: "/healthz",
			status: http.StatusMethodNotAllowed, code: problem.CodeMethodNotAllowed, allow: "GET, HEAD",
		},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			t.Parallel()

			rec := serve(t, router, tt.method, tt.path)
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d", rec.Code, tt.status)
			}
			if ct := rec.Header().Get("Content-Type"); ct != problem.ContentType {
				t.Errorf("Content-Type = %q, want %q", ct, problem.ContentType)
			}
			if got := rec.Header().Get("Allow"); got != tt.allow {
				t.Errorf("Allow = %q, want %q", got, tt.allow)
			}

			var p problem.Problem
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			if p.Code != tt.code || p.Instance != tt.path || p.RequestID == "" {
				t.Errorf("problem = %+v", p)
			}
		})
	}
}

func TestRouterPassesThroughCanonicalRedirects(t *testing.T) {
	t.Parallel()

	rec := serve(t, newTestRouter(), http.MethodGet, "/v1//movies")
	isRedirect := rec.Code >= http.StatusMultipleChoices && rec.Code < http.StatusBadRequest
	if !isRedirect || rec.Header().Get("Location") != "/v1/movies" {
		t.Errorf("got %d Location=%q, want a redirect to /v1/movies", rec.Code, rec.Header().Get("Location"))
	}
}
