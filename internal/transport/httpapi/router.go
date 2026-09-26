// Package httpapi assembles the HTTP API: routes, middleware, and handlers.
package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/handler"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/middleware"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

// RouterDeps holds everything the router needs to build its handlers.
type RouterDeps struct {
	Logger   *slog.Logger
	Tokens   middleware.TokenVerifier
	Health   *handler.Health
	Catalog  *handler.Catalog
	Auth     *handler.Auth
	Bookings *handler.Bookings
	Admin    *handler.Admin
}

// NewRouter registers all routes and wraps them in the shared middleware stack.
func NewRouter(d RouterDeps) http.Handler {
	mux := http.NewServeMux()

	// Access levels. Routes registered with mux.HandleFunc directly are public.
	authenticate := middleware.Authenticate(d.Tokens, d.Logger)
	user := func(h http.HandlerFunc) http.Handler {
		return middleware.Chain(h, authenticate)
	}
	admin := func(h http.HandlerFunc) http.Handler {
		return middleware.Chain(h, authenticate, middleware.RequireRole(domain.RoleAdmin))
	}

	mux.HandleFunc("GET /healthz", d.Health.Live)
	mux.HandleFunc("GET /readyz", d.Health.Ready)

	mux.HandleFunc("POST /v1/auth/register", d.Auth.Register)
	mux.HandleFunc("POST /v1/auth/login", d.Auth.Login)
	mux.Handle("GET /v1/me", user(d.Auth.Me))

	mux.HandleFunc("GET /v1/movies", d.Catalog.ListMovies)
	mux.HandleFunc("GET /v1/movies/{movieID}", d.Catalog.GetMovie)
	mux.HandleFunc("GET /v1/showtimes", d.Catalog.Schedule)
	mux.HandleFunc("GET /v1/showtimes/{showtimeID}", d.Catalog.GetShowtime)
	mux.HandleFunc("GET /v1/showtimes/{showtimeID}/seats", d.Catalog.SeatMap)

	mux.Handle("POST /v1/bookings", user(d.Bookings.Create))
	mux.Handle("GET /v1/bookings", user(d.Bookings.List))
	mux.Handle("GET /v1/bookings/{bookingID}", user(d.Bookings.Get))
	mux.Handle("DELETE /v1/bookings/{bookingID}", user(d.Bookings.Cancel))

	mux.Handle("POST /v1/admin/movies", admin(d.Admin.CreateMovie))

	// Order matters: RequestID is outermost so every log line carries the ID, and Recover is innermost so that a
	// recovered panic is still logged by AccessLog with its 500 status.
	return middleware.Chain(problemFallback(mux),
		middleware.RequestID,
		middleware.AccessLog(d.Logger),
		middleware.Recover(d.Logger),
	)
}

// problemFallback serves mux but replaces its plain-text "404 page not found" and "405 method not allowed"
// responses with problem details, keeping the Allow header of a 405.
func problemFallback(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}

		// No route matched. Let the mux decide between 404 and 405, then rewrite its response.
		cw := &captureWriter{header: http.Header{}, status: http.StatusOK}
		mux.ServeHTTP(cw, r)

		if cw.status < http.StatusBadRequest { // not an error (for example a redirect): pass it through unchanged
			for k, v := range cw.header {
				w.Header()[k] = v
			}
			w.WriteHeader(cw.status)
			_, _ = w.Write(cw.body.Bytes())
			return
		}

		if allow := cw.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		problem.Write(w, r, problem.New(cw.status, codeForStatus(cw.status), ""))
	})
}

func codeForStatus(status int) string {
	switch status {
	case http.StatusNotFound:
		return problem.CodeNotFound
	case http.StatusMethodNotAllowed:
		return problem.CodeMethodNotAllowed
	default:
		return "HTTP_" + strconv.Itoa(status)
	}
}

// captureWriter buffers a response so it can be inspected before anything reaches the client.
type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *captureWriter) Header() http.Header         { return c.header }
func (c *captureWriter) Write(b []byte) (int, error) { return c.body.Write(b) }
func (c *captureWriter) WriteHeader(status int)      { c.status = status }
