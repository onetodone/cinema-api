// Package httpapi assembles the HTTP API: routes, middleware, and handlers.
package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
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
	Payments *handler.Payments
	Admin    *handler.Admin

	// Metrics records request durations and what the request guards do. It may be nil only when no guard is set;
	// then request durations are not recorded. The metrics are served on a listener of their own (see
	// internal/app), not by this router, so that they stay off the public network.
	Metrics *metrics.Metrics

	// Request guards backed by Redis. A nil store still requires a well-formed Idempotency-Key where one is
	// required, but stores nothing; a nil limiter turns its limit off.
	Idempotency       middleware.IdempotencyStore
	BookingLimiter    middleware.RateLimiter // booking attempts per user
	AuthIPLimiter     middleware.RateLimiter // login and registration attempts per client address
	LoginEmailLimiter middleware.RateLimiter // login attempts per account email
	RefreshLimiter    middleware.RateLimiter // refreshes per session
	// TrustedProxies are the networks whose X-Forwarded-For names the client address.
	TrustedProxies []netip.Prefix
	// Revocations lists the sessions that ended before they expired; their access tokens are refused. nil checks
	// nothing: such tokens work until they expire.
	Revocations middleware.RevocationList
}

// NewRouter registers all routes and wraps them in the shared middleware stack.
func NewRouter(d RouterDeps) http.Handler {
	mux, _ := newMux(d)

	// Order matters: RequestID is outermost so every log line carries the ID, and Recover is innermost so that a
	// recovered panic is still logged by AccessLog, and measured by Instrument, with its 500 status.
	mws := []middleware.Middleware{middleware.RequestID, middleware.AccessLog(d.Logger)}
	if d.Metrics != nil {
		mws = append(mws, middleware.Instrument(d.Metrics))
	}
	mws = append(mws, middleware.Recover(d.Logger))
	return middleware.Chain(problemFallback(mux), mws...)
}

// newMux registers every route on a new ServeMux, and returns it with the route patterns in registration order.
// api/openapi.yaml must document exactly these routes; a test checks that.
func newMux(d RouterDeps) (*http.ServeMux, []string) {
	mux := http.NewServeMux()
	var patterns []string
	handle := func(pattern string, h http.Handler) {
		mux.Handle(pattern, h)
		patterns = append(patterns, pattern)
	}

	// Access levels.
	authenticate := middleware.Authenticate(d.Tokens, d.Revocations, d.Logger)
	public := func(h http.HandlerFunc, mws ...middleware.Middleware) http.Handler {
		return middleware.Chain(h, mws...)
	}
	user := func(h http.HandlerFunc, mws ...middleware.Middleware) http.Handler {
		return middleware.Chain(h, append([]middleware.Middleware{authenticate}, mws...)...)
	}
	admin := func(h http.HandlerFunc) http.Handler {
		return middleware.Chain(h, authenticate, middleware.RequireRole(domain.RoleAdmin))
	}

	// Request guards. Limits come first, so that a rejected attempt costs one Redis call and nothing else.
	limit := func(name string, limiter middleware.RateLimiter, key middleware.RateKey) middleware.Middleware {
		if limiter == nil {
			return func(next http.Handler) http.Handler { return next }
		}
		return middleware.RateLimit(name, limiter, key, d.Metrics)
	}
	limitAuthByIP := limit(metrics.LimitAuthIP, d.AuthIPLimiter, middleware.ByClientIP(d.TrustedProxies))
	limitLoginByEmail := limit(metrics.LimitLoginEmail, d.LoginEmailLimiter, middleware.ByEmail)
	limitRefreshBySession := limit(metrics.LimitRefresh, d.RefreshLimiter,
		middleware.ByRefreshSession(handler.RefreshCookieName, d.TrustedProxies))
	limitBookings := limit(metrics.LimitBooking, d.BookingLimiter, middleware.ByUser)
	idempotent := func(required bool) middleware.Middleware {
		return middleware.Idempotency(d.Idempotency, required, d.Metrics, d.Logger)
	}

	handle("GET /healthz", public(d.Health.Live))
	handle("GET /readyz", public(d.Health.Ready))

	handle("POST /v1/auth/register", public(d.Auth.Register, limitAuthByIP))
	handle("POST /v1/auth/login", public(d.Auth.Login, limitAuthByIP, limitLoginByEmail))
	// The refresh token cookie authenticates these two; the handlers check it.
	handle("POST /v1/auth/refresh", public(d.Auth.Refresh, limitRefreshBySession))
	handle("POST /v1/auth/logout", public(d.Auth.Logout))
	handle("POST /v1/auth/logout-all", user(d.Auth.LogoutAll))
	handle("GET /v1/auth/sessions", user(d.Auth.ListSessions))
	handle("DELETE /v1/auth/sessions/{sessionID}", user(d.Auth.DeleteSession))
	handle("GET /v1/me", user(d.Auth.Me))

	handle("GET /v1/movies", public(d.Catalog.ListMovies))
	handle("GET /v1/movies/{movieID}", public(d.Catalog.GetMovie))
	handle("GET /v1/showtimes", public(d.Catalog.Schedule))
	handle("GET /v1/showtimes/{showtimeID}", public(d.Catalog.GetShowtime))
	handle("GET /v1/showtimes/{showtimeID}/seats", public(d.Catalog.SeatMap))

	handle("POST /v1/bookings", user(d.Bookings.Create, limitBookings, idempotent(false)))
	handle("GET /v1/bookings", user(d.Bookings.List))
	handle("GET /v1/bookings/{bookingID}", user(d.Bookings.Get))
	handle("DELETE /v1/bookings/{bookingID}", user(d.Bookings.Cancel))

	handle("GET /v1/payment-methods", public(d.Payments.ListMethods))
	handle("POST /v1/bookings/{bookingID}/payments", user(d.Payments.Pay, idempotent(true)))

	handle("POST /v1/admin/movies", admin(d.Admin.CreateMovie))
	handle("POST /v1/admin/halls", admin(d.Admin.CreateHall))
	handle("POST /v1/admin/showtimes", admin(d.Admin.CreateShowtime))

	return mux, patterns
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
