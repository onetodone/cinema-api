package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/payment"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/service/auth"
	"github.com/onetodone/cinema-api/internal/service/booking"
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

func (emptyCatalog) ListGenres(context.Context) ([]domain.Genre, error) {
	return nil, nil
}

func (emptyCatalog) GetGenre(_ context.Context, id int64) (domain.Genre, error) {
	return domain.Genre{ID: id}, nil
}

// oneUser knows a single account and accepts any password for it.
type oneUser struct{ user domain.User }

func (o oneUser) Register(context.Context, string, string) (domain.User, error) {
	return o.user, nil
}

func (o oneUser) User(context.Context, uuid.UUID) (domain.User, error) {
	return o.user, nil
}

// issuingSessions starts a new session for every login and every well-formed refresh token, and signs real access
// tokens for them.
type issuingSessions struct{ user domain.User }

func (s issuingSessions) Login(context.Context, string, string, auth.Client) (auth.Grant, error) {
	return s.grant()
}

func (s issuingSessions) Refresh(_ context.Context, token string, _ auth.Client) (auth.Grant, auth.RefreshResult, error) {
	if _, ok := domain.ParseRefreshToken(token); !ok {
		return auth.Grant{}, auth.RefreshInvalid, domain.Unauthenticated(domain.CodeRefreshInvalid, "log in again")
	}
	g, err := s.grant()
	return g, auth.RefreshRotated, err
}

func (issuingSessions) Logout(context.Context, string) (bool, error)              { return true, nil }
func (issuingSessions) LogoutAll(context.Context, uuid.UUID) (int, error)         { return 1, nil }
func (issuingSessions) List(context.Context, uuid.UUID) ([]domain.Session, error) { return nil, nil }
func (issuingSessions) Revoke(context.Context, uuid.UUID, uuid.UUID) error        { return nil }

func (s issuingSessions) grant() (auth.Grant, error) {
	id := uuid.NewV7()
	access, err := testTokens.Issue(domain.Principal{UserID: s.user.ID, Role: s.user.Role, SessionID: id})
	return auth.Grant{
		User:    s.user,
		Session: domain.Session{ID: id, UserID: s.user.ID, ExpiresAt: time.Now().Add(time.Hour)},
		Access:  access,
		Refresh: domain.NewRefreshToken(id),
	}, err
}

// noBookings holds every seat it is asked for and knows no booking.
type noBookings struct{}

func (noBookings) Create(_ context.Context, userID uuid.UUID, nb domain.NewBooking) (domain.Booking, error) {
	return domain.Booking{ID: uuid.NewV7(), UserID: userID, Showtime: domain.ShowtimeRef{ID: nb.ShowtimeID}}, nil
}

func (noBookings) Get(_ context.Context, _, id uuid.UUID) (domain.Booking, error) {
	return domain.Booking{}, domain.BookingNotFound(id)
}

func (noBookings) List(context.Context, uuid.UUID, booking.ListQuery) (booking.Page, error) {
	return booking.Page{}, nil
}

func (noBookings) Cancel(context.Context, uuid.UUID, uuid.UUID) error { return nil }

// paysAll settles every payment as succeeded.
type paysAll struct{}

func (paysAll) Pay(_ context.Context, userID, bookingID uuid.UUID, np domain.NewPayment) (booking.PayResult, error) {
	return booking.PayResult{
		Payment: domain.Payment{ID: uuid.NewV7(), BookingID: bookingID, Provider: np.Method, Status: domain.PaymentSucceeded},
		Booking: domain.Booking{ID: bookingID, UserID: userID, Status: domain.BookingPaid},
	}, nil
}

// localMethod offers the local test provider.
type localMethod struct{}

func (localMethod) Methods() []payment.Method {
	return []payment.Method{{ID: "local", Name: "Test card"}}
}

// echoAdmin creates everything with id 1.
type echoAdmin struct{}

func (echoAdmin) CreateMovie(_ context.Context, m domain.NewMovie) (domain.Movie, error) {
	return domain.Movie{ID: 1, Title: m.Title, DurationMin: m.DurationMin}, nil
}

func (echoAdmin) CreateHall(_ context.Context, h domain.NewHall) (domain.HallLayout, error) {
	return domain.HallLayout{Hall: domain.Hall{ID: 1, Name: h.Name}}, nil
}

func (echoAdmin) CreateShowtime(_ context.Context, ns domain.NewShowtime) (domain.Showtime, error) {
	return domain.Showtime{ID: 1, StartsAt: ns.StartsAt, EndsAt: ns.StartsAt.Add(time.Hour)}, nil
}

func (echoAdmin) SetMovieGenres(_ context.Context, movieID int64, _ []int64) (domain.Movie, error) {
	return domain.Movie{ID: movieID}, nil
}

func (echoAdmin) CreateGenre(_ context.Context, g domain.NewGenre) (domain.Genre, error) {
	return domain.Genre{ID: 1, Slug: g.Slug, Name: g.Name}, nil
}

func (echoAdmin) UpdateGenre(_ context.Context, id int64, g domain.NewGenre) (domain.Genre, error) {
	return domain.Genre{ID: id, Slug: g.Slug, Name: g.Name}, nil
}

func (echoAdmin) DeleteGenre(context.Context, int64) error { return nil }

var testTokens = func() *auth.Tokens {
	t, err := auth.NewTokens(strings.Repeat("k", auth.MinSecretBytes), time.Hour)
	if err != nil {
		panic(err)
	}
	return t
}()

func newTestRouter() http.Handler {
	return NewRouter(testRouterDeps())
}

// testRouterDeps returns router dependencies with fake services and no Redis guards.
func testRouterDeps() RouterDeps {
	logger := slog.New(slog.DiscardHandler)
	user := domain.User{ID: uuid.NewV7(), Email: "ann@example.com", Role: domain.RoleCustomer}
	m := metrics.New(prometheus.NewRegistry())
	return RouterDeps{
		Logger: logger,
		Tokens: testTokens,
		Health: handler.NewHealth(logger, time.Second,
			handler.Check{Name: "postgres", Critical: true, Probe: func(context.Context) error { return nil }},
		),
		Catalog: handler.NewCatalog(emptyCatalog{}, "USD", logger),
		Auth: handler.NewAuth(oneUser{user: user}, issuingSessions{user: user},
			handler.AuthConfig{Cookie: handler.RefreshCookie{Path: "/v1/auth", Secure: true}}, m, logger),
		Bookings: handler.NewBookings(noBookings{}, "USD", m, logger),
		Payments: handler.NewPayments(paysAll{}, localMethod{}, "USD", m, logger),
		Admin:    handler.NewAdmin(echoAdmin{}, "USD", logger),

		Metrics: m,
	}
}

func serve(t *testing.T, router http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	return serveAs(t, router, method, target, "", "")
}

// serveAs sends a request with an optional bearer token and JSON body, and headers given as name, value pairs.
func serveAs(t *testing.T, router http.Handler, method, target, token, body string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func tokenFor(t *testing.T, role domain.Role) string {
	t.Helper()
	tok, err := testTokens.Issue(domain.Principal{UserID: uuid.NewV7(), Role: role, SessionID: uuid.NewV7()})
	if err != nil {
		t.Fatal(err)
	}
	return tok.Token
}

func TestRouterServesRegisteredRoutes(t *testing.T) {
	t.Parallel()

	router := newTestRouter()
	for _, path := range []string{
		"/healthz", "/readyz",
		"/v1/movies", "/v1/movies/1", "/v1/genres", "/v1/genres/1",
		"/v1/showtimes", "/v1/showtimes/1", "/v1/showtimes/1/seats",
		"/v1/payment-methods",
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

func TestRouterEnforcesAccessLevels(t *testing.T) {
	t.Parallel()

	router := newTestRouter()
	customer, admin := tokenFor(t, domain.RoleCustomer), tokenFor(t, domain.RoleAdmin)
	credentials := `{"email":"ann@example.com","password":"correct horse"}`
	movie := `{"title":"Dune","duration_min":155}`
	hall := `{"name":"Hall 9","rows":[{"label":"A","seats":5}]}`
	genre := `{"slug":"noir","name":"Noir"}`
	genreIDs := `{"genre_ids":[1,2]}`
	showtime := `{"movie_id":1,"hall_id":1,"starts_at":"2030-01-01T19:00:00Z","base_price_cents":900}`
	seats := `{"showtime_id":1,"seat_ids":[1,2]}`
	bookingPath := "/v1/bookings/" + uuid.NewV7().String()
	pay := `{"payment_method":"local","payment_token":"tok_success"}`
	refreshCookie := handler.RefreshCookieName + "=" + domain.NewRefreshToken(uuid.NewV7()).String()
	sessionPath := "/v1/auth/sessions/" + uuid.NewV7().String()

	tests := []struct {
		name   string
		method string
		path   string
		token  string
		body   string
		key    string // Idempotency-Key
		cookie string
		status int
	}{
		{name: "register is public", method: http.MethodPost, path: "/v1/auth/register", body: credentials, status: http.StatusCreated},
		{name: "login is public", method: http.MethodPost, path: "/v1/auth/login", body: credentials, status: http.StatusOK},
		{name: "refresh needs the cookie", method: http.MethodPost, path: "/v1/auth/refresh", body: `{}`, status: http.StatusUnauthorized},
		{name: "refresh ignores bearer tokens", method: http.MethodPost, path: "/v1/auth/refresh", token: customer, body: `{}`, status: http.StatusUnauthorized},
		{name: "refresh with the cookie", method: http.MethodPost, path: "/v1/auth/refresh", body: `{}`, cookie: refreshCookie, status: http.StatusOK},
		{name: "logout is public", method: http.MethodPost, path: "/v1/auth/logout", body: `{}`, status: http.StatusNoContent},
		{name: "logout-all needs a token", method: http.MethodPost, path: "/v1/auth/logout-all", status: http.StatusUnauthorized},
		{name: "logout-all for a customer", method: http.MethodPost, path: "/v1/auth/logout-all", token: customer, status: http.StatusNoContent},
		{name: "sessions need a token", method: http.MethodGet, path: "/v1/auth/sessions", status: http.StatusUnauthorized},
		{name: "sessions for a customer", method: http.MethodGet, path: "/v1/auth/sessions", token: customer, status: http.StatusOK},
		{name: "session revoke needs a token", method: http.MethodDelete, path: sessionPath, status: http.StatusUnauthorized},
		{name: "session revoke for a customer", method: http.MethodDelete, path: sessionPath, token: customer, status: http.StatusNoContent},
		{name: "catalog is public", method: http.MethodGet, path: "/v1/movies", status: http.StatusOK},
		{name: "genres are public", method: http.MethodGet, path: "/v1/genres", status: http.StatusOK},
		{name: "a genre is public", method: http.MethodGet, path: "/v1/genres/1", status: http.StatusOK},
		{name: "me needs a token", method: http.MethodGet, path: "/v1/me", status: http.StatusUnauthorized},
		{name: "me rejects a forged token", method: http.MethodGet, path: "/v1/me", token: "forged", status: http.StatusUnauthorized},
		{name: "me for a customer", method: http.MethodGet, path: "/v1/me", token: customer, status: http.StatusOK},
		{name: "me for an admin", method: http.MethodGet, path: "/v1/me", token: admin, status: http.StatusOK},
		{name: "booking needs a token", method: http.MethodPost, path: "/v1/bookings", body: seats, status: http.StatusUnauthorized},
		{name: "booking for a customer", method: http.MethodPost, path: "/v1/bookings", token: customer, body: seats, status: http.StatusCreated},
		{name: "booking for an admin", method: http.MethodPost, path: "/v1/bookings", token: admin, body: seats, status: http.StatusCreated},
		{name: "booking list needs a token", method: http.MethodGet, path: "/v1/bookings", status: http.StatusUnauthorized},
		{name: "booking list for a customer", method: http.MethodGet, path: "/v1/bookings", token: customer, status: http.StatusOK},
		{name: "booking read needs a token", method: http.MethodGet, path: bookingPath, status: http.StatusUnauthorized},
		{name: "booking read for a customer", method: http.MethodGet, path: bookingPath, token: customer, status: http.StatusNotFound},
		{name: "booking cancel needs a token", method: http.MethodDelete, path: bookingPath, status: http.StatusUnauthorized},
		{name: "booking cancel for a customer", method: http.MethodDelete, path: bookingPath, token: customer, status: http.StatusNoContent},
		{name: "payment methods are public", method: http.MethodGet, path: "/v1/payment-methods", status: http.StatusOK},
		{name: "payment needs a token", method: http.MethodPost, path: bookingPath + "/payments", body: pay, status: http.StatusUnauthorized},
		{name: "payment for a customer", method: http.MethodPost, path: bookingPath + "/payments", token: customer, body: pay, key: "k1", status: http.StatusOK},
		{name: "payment needs an idempotency key", method: http.MethodPost, path: bookingPath + "/payments", token: customer, body: pay, status: http.StatusBadRequest},
		{name: "metrics are not on the public port", method: http.MethodGet, path: "/metrics", status: http.StatusNotFound},
		{name: "admin route needs a token", method: http.MethodPost, path: "/v1/admin/movies", body: movie, status: http.StatusUnauthorized},
		{name: "admin route refuses customers", method: http.MethodPost, path: "/v1/admin/movies", token: customer, body: movie, status: http.StatusForbidden},
		{name: "admin route for an admin", method: http.MethodPost, path: "/v1/admin/movies", token: admin, body: movie, status: http.StatusCreated},
		{name: "hall needs a token", method: http.MethodPost, path: "/v1/admin/halls", body: hall, status: http.StatusUnauthorized},
		{name: "hall refuses customers", method: http.MethodPost, path: "/v1/admin/halls", token: customer, body: hall, status: http.StatusForbidden},
		{name: "hall for an admin", method: http.MethodPost, path: "/v1/admin/halls", token: admin, body: hall, status: http.StatusCreated},
		{name: "showtime needs a token", method: http.MethodPost, path: "/v1/admin/showtimes", body: showtime, status: http.StatusUnauthorized},
		{name: "showtime refuses customers", method: http.MethodPost, path: "/v1/admin/showtimes", token: customer, body: showtime, status: http.StatusForbidden},
		{name: "showtime for an admin", method: http.MethodPost, path: "/v1/admin/showtimes", token: admin, body: showtime, status: http.StatusCreated},
		{name: "movie genres need a token", method: http.MethodPut, path: "/v1/admin/movies/1/genres", body: genreIDs, status: http.StatusUnauthorized},
		{name: "movie genres refuse customers", method: http.MethodPut, path: "/v1/admin/movies/1/genres", token: customer, body: genreIDs, status: http.StatusForbidden},
		{name: "movie genres for an admin", method: http.MethodPut, path: "/v1/admin/movies/1/genres", token: admin, body: genreIDs, status: http.StatusOK},
		{name: "genre create needs a token", method: http.MethodPost, path: "/v1/admin/genres", body: genre, status: http.StatusUnauthorized},
		{name: "genre create refuses customers", method: http.MethodPost, path: "/v1/admin/genres", token: customer, body: genre, status: http.StatusForbidden},
		{name: "genre create for an admin", method: http.MethodPost, path: "/v1/admin/genres", token: admin, body: genre, status: http.StatusCreated},
		{name: "genre update needs a token", method: http.MethodPut, path: "/v1/admin/genres/1", body: genre, status: http.StatusUnauthorized},
		{name: "genre update refuses customers", method: http.MethodPut, path: "/v1/admin/genres/1", token: customer, body: genre, status: http.StatusForbidden},
		{name: "genre update for an admin", method: http.MethodPut, path: "/v1/admin/genres/1", token: admin, body: genre, status: http.StatusOK},
		{name: "genre delete needs a token", method: http.MethodDelete, path: "/v1/admin/genres/1", status: http.StatusUnauthorized},
		{name: "genre delete refuses customers", method: http.MethodDelete, path: "/v1/admin/genres/1", token: customer, status: http.StatusForbidden},
		{name: "genre delete for an admin", method: http.MethodDelete, path: "/v1/admin/genres/1", token: admin, status: http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var header []string
			if tt.key != "" {
				header = append(header, "Idempotency-Key", tt.key)
			}
			if tt.cookie != "" {
				header = append(header, "Cookie", tt.cookie)
			}
			rec := serveAs(t, router, tt.method, tt.path, tt.token, tt.body, header...)
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tt.status, rec.Body.String())
			}
			if rec.Code == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 without a WWW-Authenticate challenge")
			}
		})
	}
}

// revokedSessions is a revocation list of the sessions it holds.
type revokedSessions map[uuid.UUID]bool

func (r revokedSessions) Revoked(_ context.Context, sessionID uuid.UUID) (bool, error) {
	return r[sessionID], nil
}

func TestRouterRefusesTokensOfRevokedSessions(t *testing.T) {
	t.Parallel()

	issue := func(role domain.Role, sessionID uuid.UUID) string {
		tok, err := testTokens.Issue(domain.Principal{UserID: uuid.NewV7(), Role: role, SessionID: sessionID})
		if err != nil {
			t.Fatal(err)
		}
		return tok.Token
	}
	revoked := uuid.NewV7()
	deps := testRouterDeps()
	deps.Revocations = revokedSessions{revoked: true}
	router := NewRouter(deps)
	customer, admin := issue(domain.RoleCustomer, revoked), issue(domain.RoleAdmin, revoked)

	for _, tt := range []struct{ method, path, token, body string }{
		{method: http.MethodGet, path: "/v1/me", token: customer},
		{method: http.MethodGet, path: "/v1/bookings", token: customer},
		{method: http.MethodGet, path: "/v1/auth/sessions", token: customer},
		{method: http.MethodPost, path: "/v1/auth/logout-all", token: customer},
		{method: http.MethodPost, path: "/v1/admin/movies", token: admin, body: `{"title":"Dune","duration_min":155}`},
	} {
		rec := serveAs(t, router, tt.method, tt.path, tt.token, tt.body)
		var p problem.Problem
		_ = json.Unmarshal(rec.Body.Bytes(), &p)
		if rec.Code != http.StatusUnauthorized || p.Code != domain.CodeInvalidToken || rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s with a token of a revoked session = %d %s, want 401 INVALID_TOKEN", tt.method, tt.path, rec.Code, rec.Body.String())
		}
	}
	if rec := serveAs(t, router, http.MethodGet, "/v1/me", issue(domain.RoleCustomer, uuid.NewV7()), ""); rec.Code != http.StatusOK {
		t.Errorf("a token of another session = %d, want 200", rec.Code)
	}
	// Public routes do not look at bearer tokens at all.
	if rec := serveAs(t, router, http.MethodGet, "/v1/movies", customer, ""); rec.Code != http.StatusOK {
		t.Errorf("a public route with a token of a revoked session = %d, want 200", rec.Code)
	}
}

func TestRouterLoginTokenOpensProtectedRoutes(t *testing.T) {
	t.Parallel()

	router := newTestRouter()
	rec := serveAs(t, router, http.MethodPost, "/v1/auth/login", "", `{"email":"ann@example.com","password":"x"}`)
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.AccessToken == "" {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}

	if rec := serveAs(t, router, http.MethodGet, "/v1/me", body.AccessToken, ""); rec.Code != http.StatusOK {
		t.Errorf("GET /v1/me with the login token: status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestRouterMeasuresRequestsByRoute(t *testing.T) {
	t.Parallel()

	deps := testRouterDeps()
	router := NewRouter(deps)
	serve(t, router, http.MethodGet, "/v1/showtimes/1/seats")
	serve(t, router, http.MethodGet, "/v1/showtimes/2/seats")
	serve(t, router, http.MethodGet, "/v1/showtimes/x/seats")
	serve(t, router, http.MethodGet, "/nope")
	serve(t, router, http.MethodPost, "/v1/movies")

	for _, tt := range []struct {
		route, code string
		want        uint64
	}{
		{route: "GET /v1/showtimes/{showtimeID}/seats", code: "200", want: 2},
		{route: "GET /v1/showtimes/{showtimeID}/seats", code: "400", want: 1},
		{route: metrics.RouteUnmatched, code: "404", want: 1},
		{route: metrics.RouteUnmatched, code: "405", want: 1},
	} {
		if got := sampleCount(t, deps.Metrics, tt.route, tt.code); got != tt.want {
			t.Errorf("requests measured for %s %s = %d, want %d", tt.route, tt.code, got, tt.want)
		}
	}
}

// sampleCount returns how many requests the duration histogram observed for a route and status code.
func sampleCount(t *testing.T, m *metrics.Metrics, route, code string) uint64 {
	t.Helper()
	obs, err := m.HTTPRequestDuration.GetMetricWithLabelValues(route, code)
	if err != nil {
		t.Fatal(err)
	}
	var out dto.Metric
	if err := obs.(prometheus.Metric).Write(&out); err != nil {
		t.Fatal(err)
	}
	return out.GetHistogram().GetSampleCount()
}
