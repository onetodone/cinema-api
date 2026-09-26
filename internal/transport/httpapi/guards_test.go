package httpapi

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/middleware"
)

// recordingLimiter records the keys it is asked about and denies every attempt once deny is set.
type recordingLimiter struct {
	mu   sync.Mutex
	keys []string
	deny bool
}

func (l *recordingLimiter) Allow(_ context.Context, key string) (bool, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, key)
	return !l.deny, 30 * time.Second, nil
}

func (l *recordingLimiter) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.keys...)
}

// memIdempotency is an in-memory middleware.IdempotencyStore that ignores TTLs.
type memIdempotency struct {
	mu      sync.Mutex
	records map[string][]byte
}

func (m *memIdempotency) Claim(_ context.Context, key, _ string, record []byte, _ time.Duration) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.records[key]; ok {
		return existing, false, nil
	}
	m.records[key] = record
	return nil, true, nil
}

func (m *memIdempotency) Complete(_ context.Context, key, _ string, record []byte, _ time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records[key] = record
	return true, nil
}

func (m *memIdempotency) Release(_ context.Context, key, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.records, key)
	return nil
}

func TestRouterRateLimitsAuthAndBookings(t *testing.T) {
	t.Parallel()

	deps := testRouterDeps()
	book, authIP, loginEmail := &recordingLimiter{}, &recordingLimiter{}, &recordingLimiter{}
	deps.BookingLimiter, deps.AuthIPLimiter, deps.LoginEmailLimiter = book, authIP, loginEmail
	router := NewRouter(deps)
	customer := tokenFor(t, domain.RoleCustomer)

	serveAs(t, router, http.MethodPost, "/v1/auth/register", "", `{"email":"ann@example.com","password":"correct horse"}`)
	serveAs(t, router, http.MethodPost, "/v1/auth/login", "", `{"email":" Ann@Example.com ","password":"x"}`)
	serveAs(t, router, http.MethodPost, "/v1/auth/login", "", `{"email":"ann@example.com","password":"y"}`)
	serveAs(t, router, http.MethodPost, "/v1/bookings", customer, `{"showtime_id":1,"seat_ids":[1]}`)
	serve(t, router, http.MethodGet, "/v1/movies")

	// httptest requests come from 192.0.2.1.
	if got := authIP.seen(); len(got) != 3 || got[0] != "192.0.2.1" {
		t.Errorf("auth IP limit keys = %v, want 192.0.2.1 for the registration and both logins", got)
	}
	if got := loginEmail.seen(); len(got) != 2 || got[0] != got[1] || len(got[0]) != 32 {
		t.Errorf("login email limit keys = %v, want one hashed key for both spellings of the address", got)
	}
	if got := book.seen(); len(got) != 1 || len(got[0]) != 36 {
		t.Errorf("booking limit keys = %v, want the caller's user id", got)
	}

	book.deny = true
	rec := serveAs(t, router, http.MethodPost, "/v1/bookings", customer, `{"showtime_id":1,"seat_ids":[1]}`)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "30" {
		t.Errorf("booking over the limit = %d, Retry-After %q; want 429 and 30", rec.Code, rec.Header().Get("Retry-After"))
	}
	if n := testutil.ToFloat64(deps.Metrics.RateLimitRejections.WithLabelValues(metrics.LimitBooking)); n != 1 {
		t.Errorf("booking rejections = %v, want 1", n)
	}
}

func TestRouterReplaysIdempotentBookings(t *testing.T) {
	t.Parallel()

	deps := testRouterDeps()
	deps.Idempotency = &memIdempotency{records: map[string][]byte{}}
	router := NewRouter(deps)
	customer := tokenFor(t, domain.RoleCustomer)
	body := `{"showtime_id":1,"seat_ids":[1]}`

	first := serveAs(t, router, http.MethodPost, "/v1/bookings", customer, body, "Idempotency-Key", "k1")
	again := serveAs(t, router, http.MethodPost, "/v1/bookings", customer, body, "Idempotency-Key", "k1")
	other := serveAs(t, router, http.MethodPost, "/v1/bookings", customer, body, "Idempotency-Key", "k2")

	// The fake service gives every booking a new id, so equal bodies prove a replay.
	if first.Code != http.StatusCreated || again.Code != http.StatusCreated || again.Body.String() != first.Body.String() {
		t.Fatalf("first = %d %s, again = %d %s", first.Code, first.Body, again.Code, again.Body)
	}
	if again.Header().Get(middleware.IdempotentReplayedHeader) != "true" || again.Header().Get("Location") != first.Header().Get("Location") {
		t.Errorf("replay headers = %v", again.Header())
	}
	if other.Body.String() == first.Body.String() {
		t.Error("another key got the same booking")
	}
}
