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

// oneUser knows a single account and accepts any password for it.
type oneUser struct{ user domain.User }

func (o oneUser) Register(context.Context, string, string) (domain.User, error) {
	return o.user, nil
}

func (o oneUser) Authenticate(context.Context, string, string) (domain.User, error) {
	return o.user, nil
}

func (o oneUser) User(context.Context, uuid.UUID) (domain.User, error) {
	return o.user, nil
}

// noBookings holds every seat it is asked for and knows no booking.
type noBookings struct{}

func (noBookings) Create(_ context.Context, userID uuid.UUID, nb domain.NewBooking) (domain.Booking, error) {
	return domain.Booking{ID: uuid.NewV7(), UserID: userID, Showtime: domain.ShowtimeRef{ID: nb.ShowtimeID}}, nil
}

func (noBookings) Get(_ context.Context, _, id uuid.UUID) (domain.Booking, error) {
	return domain.Booking{}, domain.BookingNotFound(id)
}

func (noBookings) List(context.Context, uuid.UUID, uuid.UUID, int) (booking.Page, error) {
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

// echoAdmin creates every movie with id 1.
type echoAdmin struct{}

func (echoAdmin) CreateMovie(_ context.Context, m domain.NewMovie) (domain.Movie, error) {
	return domain.Movie{ID: 1, Title: m.Title, DurationMin: m.DurationMin}, nil
}

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
	registry := prometheus.NewRegistry()
	return RouterDeps{
		Logger: logger,
		Tokens: testTokens,
		Health: handler.NewHealth(logger, time.Second,
			handler.Check{Name: "postgres", Critical: true, Probe: func(context.Context) error { return nil }},
		),
		Catalog:  handler.NewCatalog(emptyCatalog{}, "USD", logger),
		Auth:     handler.NewAuth(oneUser{user: user}, testTokens, logger),
		Bookings: handler.NewBookings(noBookings{}, "USD", logger),
		Payments: handler.NewPayments(paysAll{}, localMethod{}, "USD", logger),
		Admin:    handler.NewAdmin(echoAdmin{}, logger),

		Metrics:        metrics.New(registry),
		MetricsHandler: metrics.Handler(registry),
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
	tok, err := testTokens.Issue(domain.Principal{UserID: uuid.NewV7(), Role: role})
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
		"/v1/movies", "/v1/movies/1",
		"/v1/showtimes", "/v1/showtimes/1", "/v1/showtimes/1/seats",
		"/v1/payment-methods",
		"/metrics",
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
	seats := `{"showtime_id":1,"seat_ids":[1,2]}`
	bookingPath := "/v1/bookings/" + uuid.NewV7().String()
	pay := `{"payment_method":"local","payment_token":"tok_success"}`

	tests := []struct {
		name   string
		method string
		path   string
		token  string
		body   string
		key    string // Idempotency-Key
		status int
	}{
		{name: "register is public", method: http.MethodPost, path: "/v1/auth/register", body: credentials, status: http.StatusCreated},
		{name: "login is public", method: http.MethodPost, path: "/v1/auth/login", body: credentials, status: http.StatusOK},
		{name: "catalog is public", method: http.MethodGet, path: "/v1/movies", status: http.StatusOK},
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
		{name: "metrics are public", method: http.MethodGet, path: "/metrics", status: http.StatusOK},
		{name: "admin route needs a token", method: http.MethodPost, path: "/v1/admin/movies", body: movie, status: http.StatusUnauthorized},
		{name: "admin route refuses customers", method: http.MethodPost, path: "/v1/admin/movies", token: customer, body: movie, status: http.StatusForbidden},
		{name: "admin route for an admin", method: http.MethodPost, path: "/v1/admin/movies", token: admin, body: movie, status: http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var header []string
			if tt.key != "" {
				header = []string{"Idempotency-Key", tt.key}
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
