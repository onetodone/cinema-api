//go:build integration

package integration

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
	"uuid"

	"golang.org/x/crypto/bcrypt"

	"github.com/onetodone/cinema-api/internal/config"
	"github.com/onetodone/cinema-api/internal/repository/postgres"
	"github.com/onetodone/cinema-api/internal/service/admin"
	"github.com/onetodone/cinema-api/internal/service/auth"
	"github.com/onetodone/cinema-api/internal/service/catalog"
	"github.com/onetodone/cinema-api/internal/transport/httpapi"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/handler"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/middleware"
)

// Cache TTLs of the tests: long enough that only invalidation can make a test see a change in time.
const (
	testSeatMapTTL  = time.Minute
	testScheduleTTL = time.Minute
)

// apiOptions configure newAPIServerWith.
type apiOptions struct {
	redis   *redisEnv              // nil: the test Redis, with a key prefix of its own
	limits  config.RateLimitConfig // zero: no rate limits
	trusted []netip.Prefix
}

// newAPIServer serves the complete router over the fixture's database and the test Redis, wired as internal/app
// wires it, without rate limits.
func newAPIServer(t *testing.T, f *fixture) apiClient {
	t.Helper()
	return newAPIServerWith(t, f, apiOptions{})
}

func newAPIServerWith(t *testing.T, f *fixture, opts apiOptions) apiClient {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	env := opts.redis
	if env == nil {
		env = newRedisEnv(t)
	}

	tokens, err := auth.NewTokens(strings.Repeat("k", auth.MinSecretBytes), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	authSvc, err := auth.New(postgres.NewUsers(f.pool), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	providers, _ := testProviders(t)
	cache := env.store.CatalogCache(testSeatMapTTL, testScheduleTTL)
	bookingSvc := newBookingService(f.pool, 3*time.Second, providers, env.metrics, logger, withRedis(env)...)
	limiter := func(name string, perMinute int) middleware.RateLimiter {
		if perMinute == 0 {
			return nil
		}
		return env.store.RateLimiter(name, perMinute, time.Minute)
	}

	router := httpapi.NewRouter(httpapi.RouterDeps{
		Logger:   logger,
		Tokens:   tokens,
		Health:   handler.NewHealth(logger, time.Second),
		Catalog:  handler.NewCatalog(catalog.New(f.catalog, time.UTC, catalog.WithCache(cache)), "USD", logger),
		Auth:     handler.NewAuth(authSvc, tokens, logger),
		Bookings: handler.NewBookings(bookingSvc, "USD", env.metrics, logger),
		Payments: handler.NewPayments(bookingSvc, providers, "USD", env.metrics, logger),
		Admin:    handler.NewAdmin(admin.New(f.catalog, time.UTC, admin.WithScheduleCache(cache)), "USD", logger),

		Metrics:           env.metrics,
		Idempotency:       env.store.Idempotency(),
		BookingLimiter:    limiter("book", opts.limits.BookingPerMin),
		AuthIPLimiter:     limiter("auth-ip", opts.limits.AuthIPPerMin),
		LoginEmailLimiter: limiter("login-email", opts.limits.LoginEmailPerMin),
		TrustedProxies:    opts.trusted,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return apiClient{t: t, srv: srv, redis: env}
}

// withKey returns request headers with an Idempotency-Key, a new one if key is empty.
func withKey(key string) http.Header {
	if key == "" {
		key = uuid.NewV7().String()
	}
	return http.Header{"Idempotency-Key": {key}}
}

// signUp registers an account and returns an access token for it.
func (c apiClient) signUp(email string) string {
	c.t.Helper()
	creds := map[string]string{"email": email, "password": "correct horse"}
	if r := c.do(http.MethodPost, "/v1/auth/register", "", creds); r.status != http.StatusCreated {
		c.t.Fatalf("register %s = %d %v", email, r.status, r.body)
	}
	r := c.do(http.MethodPost, "/v1/auth/login", "", creds)
	token, _ := r.body["access_token"].(string)
	if token == "" {
		c.t.Fatalf("login %s = %d %v", email, r.status, r.body)
	}
	return token
}

// seatStatuses reads the seat map over HTTP and returns each seat's status by id.
func (c apiClient) seatStatuses(showtimeID int64) map[float64]string {
	c.t.Helper()
	r := c.do(http.MethodGet, fmt.Sprintf("/v1/showtimes/%d/seats", showtimeID), "", nil)
	if r.status != http.StatusOK {
		c.t.Fatalf("seat map = %d %v", r.status, r.body)
	}
	out := map[float64]string{}
	seats, _ := r.body["seats"].([]any)
	for _, s := range seats {
		seat, _ := s.(map[string]any)
		id, _ := seat["id"].(float64)
		out[id], _ = seat["status"].(string)
	}
	return out
}

// TestBookingAPI drives the booking endpoints through the real router, services, and database.
func TestBookingAPI(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	st := f.showtime(t, f.dune, f.hall, base)
	seats := seatIDsByLabel(t, f.pool, f.hall.ID)
	a1, b1 := seats["A1"], seats["B1"]
	api := newAPIServer(t, f)
	ann, bob := api.signUp("ann@example.com"), api.signUp("bob@example.com")

	body := map[string]any{"showtime_id": st.ID, "seat_ids": []int64{b1, a1}}
	if r := api.do(http.MethodPost, "/v1/bookings", "", body); r.status != http.StatusUnauthorized {
		t.Errorf("booking without a token = %d %v", r.status, r.body)
	}

	created := api.do(http.MethodPost, "/v1/bookings", ann, body)
	if created.status != http.StatusCreated || created.body["status"] != "pending" || created.body["total_cents"] != 2500.0 {
		t.Fatalf("create = %d %v", created.status, created.body)
	}
	location := created.header.Get("Location")
	if location != "/v1/bookings/"+created.body["id"].(string) {
		t.Errorf("Location = %q", location)
	}
	statuses := api.seatStatuses(st.ID)
	if statuses[float64(a1)] != "held" || statuses[float64(b1)] != "held" || statuses[float64(seats["A2"])] != "available" {
		t.Errorf("seat map after booking = %v", statuses)
	}

	conflict := api.do(http.MethodPost, "/v1/bookings", bob,
		map[string]any{"showtime_id": st.ID, "seat_ids": []int64{seats["A2"], a1}})
	if conflict.status != http.StatusConflict || conflict.code() != "SEAT_UNAVAILABLE" {
		t.Fatalf("competing booking = %d %v", conflict.status, conflict.body)
	}
	if ids, _ := conflict.body["unavailable_seat_ids"].([]any); len(ids) != 1 || ids[0] != float64(a1) {
		t.Errorf("unavailable_seat_ids = %v, want [%d]", conflict.body["unavailable_seat_ids"], a1)
	}

	invalid := api.do(http.MethodPost, "/v1/bookings", bob, map[string]any{"showtime_id": 0, "seat_ids": []int64{}})
	if invalid.status != http.StatusBadRequest || invalid.code() != "VALIDATION_FAILED" {
		t.Errorf("invalid booking = %d %v", invalid.status, invalid.body)
	}
	unknown := api.do(http.MethodPost, "/v1/bookings", bob, map[string]any{"showtime_id": st.ID, "seat_ids": []int64{999_999}})
	if unknown.status != http.StatusUnprocessableEntity || unknown.code() != "UNKNOWN_SEAT" {
		t.Errorf("unknown seat = %d %v", unknown.status, unknown.body)
	}

	if r := api.do(http.MethodGet, location, ann, nil); r.status != http.StatusOK || r.body["id"] != created.body["id"] {
		t.Errorf("GET own booking = %d %v", r.status, r.body)
	}
	if r := api.do(http.MethodGet, location, bob, nil); r.status != http.StatusNotFound || r.code() != "BOOKING_NOT_FOUND" {
		t.Errorf("GET another user's booking = %d %v", r.status, r.body)
	}
	list := api.do(http.MethodGet, "/v1/bookings", ann, nil)
	if items, _ := list.body["items"].([]any); list.status != http.StatusOK || len(items) != 1 {
		t.Errorf("list = %d %v", list.status, list.body)
	}

	if r := api.do(http.MethodDelete, location, bob, nil); r.status != http.StatusNotFound {
		t.Errorf("DELETE by another user = %d %v", r.status, r.body)
	}
	for range 2 { // the second DELETE finds the booking canceled already and answers the same
		if r := api.do(http.MethodDelete, location, ann, nil); r.status != http.StatusNoContent {
			t.Errorf("DELETE = %d %v", r.status, r.body)
		}
	}
	if statuses := api.seatStatuses(st.ID); statuses[float64(a1)] != "available" || statuses[float64(b1)] != "available" {
		t.Errorf("seat map after cancel = %v", statuses)
	}
	if r := api.do(http.MethodGet, location, ann, nil); r.body["status"] != "canceled" {
		t.Errorf("booking after cancel = %v", r.body)
	}

	// With the seats released, bob gets A1.
	if r := api.do(http.MethodPost, "/v1/bookings", bob, map[string]any{"showtime_id": st.ID, "seat_ids": []int64{a1}}); r.status != http.StatusCreated {
		t.Errorf("bob books the released seat = %d %v", r.status, r.body)
	}
}
