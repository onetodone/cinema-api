//go:build integration

package integration

import (
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/onetodone/cinema-api/internal/config"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
)

// TestIdempotentBookingAPI retries POST /v1/bookings with an Idempotency-Key: the retry gets the first response,
// and no second booking exists.
func TestIdempotentBookingAPI(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	st := f.showtime(t, f.dune, f.hall, base)
	seats := seatIDsByLabel(t, f.pool, f.hall.ID)
	api := newAPIServer(t, f)
	ann, bob := api.signUp("ann@example.com"), api.signUp("bob@example.com")
	a1 := map[string]any{"showtime_id": st.ID, "seat_ids": []int64{seats["A1"]}}

	first := api.doWith(http.MethodPost, "/v1/bookings", ann, a1, withKey("book-1"))
	again := api.doWith(http.MethodPost, "/v1/bookings", ann, a1, withKey("book-1"))
	if first.status != http.StatusCreated || again.status != http.StatusCreated || again.body["id"] != first.body["id"] {
		t.Fatalf("first = %d %v, retry = %d %v; want the same booking twice", first.status, first.body, again.status, again.body)
	}
	if again.header.Get("Idempotent-Replayed") != "true" || again.header.Get("Location") != first.header.Get("Location") {
		t.Errorf("retry headers = %v", again.header)
	}
	if n := countRows(t, f.pool, `SELECT count(*) FROM bookings`); n != 1 {
		t.Errorf("%d bookings stored, want 1", n)
	}

	// The same key with another body is a mistake of the client.
	a2 := map[string]any{"showtime_id": st.ID, "seat_ids": []int64{seats["A2"]}}
	if r := api.doWith(http.MethodPost, "/v1/bookings", ann, a2, withKey("book-1")); r.status != http.StatusUnprocessableEntity ||
		r.code() != "IDEMPOTENCY_KEY_REUSED" {
		t.Errorf("reused key = %d %v", r.status, r.body)
	}
	// Keys belong to their caller.
	if r := api.doWith(http.MethodPost, "/v1/bookings", bob, a2, withKey("book-1")); r.status != http.StatusCreated {
		t.Errorf("bob with ann's key = %d %v", r.status, r.body)
	}
	// Without a key, a retry runs again, and the database answers.
	if r := api.do(http.MethodPost, "/v1/bookings", ann, a1); r.status != http.StatusConflict || r.code() != "SEAT_UNAVAILABLE" {
		t.Errorf("retry without a key = %d %v", r.status, r.body)
	}
	if n := testCount(api.redis.metrics.IdempotencyRequests.WithLabelValues(metrics.IdempotencyReplayed)); n != 1 {
		t.Errorf("replayed requests = %v, want 1", n)
	}
}

// TestIdempotentPaymentAPI shows why payments require an Idempotency-Key: a retry during the charge waits, a
// retry after it gets the same payment, and the provider charges once.
func TestIdempotentPaymentAPI(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	st := f.showtime(t, f.dune, f.hall, base)
	seats := seatIDsByLabel(t, f.pool, f.hall.ID)
	api := newAPIServer(t, f)
	ann := api.signUp("ann@example.com")
	book := func(label string) string {
		t.Helper()
		r := api.do(http.MethodPost, "/v1/bookings", ann, map[string]any{"showtime_id": st.ID, "seat_ids": []int64{seats[label]}})
		if r.status != http.StatusCreated {
			t.Fatalf("book %s = %d %v", label, r.status, r.body)
		}
		return r.header.Get("Location")
	}
	pay := func(location, token, key string) apiResponse {
		t.Helper()
		return api.doWith(http.MethodPost, location+"/payments", ann,
			map[string]string{"payment_method": "local", "payment_token": token}, withKey(key))
	}

	location := book("A1")
	if r := api.do(http.MethodPost, location+"/payments", ann, map[string]string{"payment_method": "local", "payment_token": "tok_success"}); r.status != http.StatusBadRequest {
		t.Errorf("payment without a key = %d %v, want 400", r.status, r.body)
	}

	// tok_slow takes testSlowDelay; a retry in the meantime is told to wait.
	var (
		wg    sync.WaitGroup
		first apiResponse
	)
	wg.Go(func() { first = pay(location, "tok_slow", "pay-1") })
	time.Sleep(testSlowDelay / 3)
	if r := pay(location, "tok_slow", "pay-1"); r.status != http.StatusConflict || r.code() != "IDEMPOTENCY_IN_PROGRESS" ||
		r.header.Get("Retry-After") != "1" {
		t.Errorf("retry during the charge = %d %v", r.status, r.body)
	}
	wg.Wait()
	paymentOf := func(r apiResponse) any {
		p, _ := r.body["payment"].(map[string]any)
		return p["id"]
	}
	if first.status != http.StatusOK || paymentOf(first) == nil {
		t.Fatalf("first payment = %d %v", first.status, first.body)
	}
	if r := pay(location, "tok_slow", "pay-1"); r.status != http.StatusOK || paymentOf(r) != paymentOf(first) ||
		r.header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("retry after the charge = %d %v; want the first payment again", r.status, r.body)
	}
	if n := countRows(t, f.pool, `SELECT count(*) FROM payments`); n != 1 {
		t.Errorf("%d payments stored, want 1", n)
	}

	// A declined payment is the final answer to its request, and is replayed.
	declined := book("A2")
	for range 2 {
		if r := pay(declined, "tok_declined", "pay-2"); r.status != http.StatusPaymentRequired {
			t.Errorf("declined payment = %d %v", r.status, r.body)
		}
	}
	// An unavailable provider is not: the retry runs again.
	for range 2 {
		if r := pay(declined, "tok_unavailable", "pay-3"); r.status != http.StatusServiceUnavailable {
			t.Errorf("unavailable provider = %d %v", r.status, r.body)
		}
	}
	var failed, unavailable int
	if err := f.pool.QueryRow(t.Context(), `
SELECT count(*) FILTER (WHERE failure_reason = 'card_declined'), count(*) FILTER (WHERE failure_reason = 'provider_unavailable')
FROM payments`).Scan(&failed, &unavailable); err != nil {
		t.Fatal(err)
	}
	if failed != 1 || unavailable != 2 {
		t.Errorf("%d declined and %d unavailable payments, want 1 (replayed) and 2 (run again)", failed, unavailable)
	}
}

// TestRateLimitsAPI checks the limits on booking and authentication attempts, and that X-Forwarded-For counts only
// behind a trusted proxy.
func TestRateLimitsAPI(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	st := f.showtime(t, f.dune, f.hall, base)
	seats := seatIDsByLabel(t, f.pool, f.hall.ID)
	// The test client connects from 127.0.0.1, which plays the load balancer.
	api := newAPIServerWith(t, f, apiOptions{
		limits:  config.RateLimitConfig{BookingPerMin: 2, AuthIPPerMin: 4, LoginEmailPerMin: 5},
		trusted: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
	})
	from := func(ip string) http.Header { return http.Header{"X-Forwarded-For": {ip}} }
	creds := func(email, password string) map[string]string {
		return map[string]string{"email": email, "password": password}
	}
	assertLimited := func(what string, r apiResponse) {
		t.Helper()
		retry, err := strconv.Atoi(r.header.Get("Retry-After"))
		if r.status != http.StatusTooManyRequests || r.code() != "RATE_LIMITED" || err != nil || retry < 1 || retry > 60 {
			t.Errorf("%s = %d %v (Retry-After %q), want 429 within a minute", what, r.status, r.body, r.header.Get("Retry-After"))
		}
	}

	// Per client address: register and login share 4 attempts a minute.
	if r := api.doWith(http.MethodPost, "/v1/auth/register", "", creds("ann@example.com", "correct horse"), from("203.0.113.1")); r.status != http.StatusCreated {
		t.Fatalf("register = %d %v", r.status, r.body)
	}
	var ann string
	for i := range 3 {
		r := api.doWith(http.MethodPost, "/v1/auth/login", "", creds("ann@example.com", "correct horse"), from("203.0.113.1"))
		if r.status != http.StatusOK {
			t.Fatalf("login %d = %d %v", i+1, r.status, r.body)
		}
		ann, _ = r.body["access_token"].(string)
	}
	assertLimited("fifth auth attempt from one address",
		api.doWith(http.MethodPost, "/v1/auth/login", "", creds("bob@example.com", "x"), from("203.0.113.1")))
	// Addresses the client adds in front are not believed; the proxy's entry is.
	assertLimited("spoofed address in front",
		api.doWith(http.MethodPost, "/v1/auth/login", "", creds("bob@example.com", "x"), from("198.51.100.9, 203.0.113.1")))

	// Per account: ann has logged in three times; two guesses from other addresses use up her five attempts, and
	// the sixth fails before the password is checked, from whatever address it comes.
	for _, ip := range []string{"203.0.113.2", "203.0.113.3"} {
		if r := api.doWith(http.MethodPost, "/v1/auth/login", "", creds("ANN@example.com", "wrong password"), from(ip)); r.status != http.StatusUnauthorized {
			t.Errorf("wrong password from %s = %d %v", ip, r.status, r.body)
		}
	}
	assertLimited("sixth login for one account",
		api.doWith(http.MethodPost, "/v1/auth/login", "", creds("ann@example.com", "correct horse"), from("203.0.113.4")))

	// Per user: two booking attempts a minute.
	for i, label := range []string{"A1", "A1"} {
		r := api.do(http.MethodPost, "/v1/bookings", ann, map[string]any{"showtime_id": st.ID, "seat_ids": []int64{seats[label]}})
		if want := []int{http.StatusCreated, http.StatusConflict}[i]; r.status != want {
			t.Errorf("booking attempt %d = %d %v, want %d", i+1, r.status, r.body, want)
		}
	}
	assertLimited("third booking attempt",
		api.do(http.MethodPost, "/v1/bookings", ann, map[string]any{"showtime_id": st.ID, "seat_ids": []int64{seats["B1"]}}))

	for limit, want := range map[string]float64{metrics.LimitAuthIP: 2, metrics.LimitLoginEmail: 1, metrics.LimitBooking: 1} {
		if n := testCount(api.redis.metrics.RateLimitRejections.WithLabelValues(limit)); n != want {
			t.Errorf("%s rejections = %v, want %v", limit, n, want)
		}
	}
}

// TestAPIWithRedisDown runs the booking flow with every Redis feature pointed at an address where no Redis listens:
// everything works, just without the gate, the caches, the limits, and replays.
func TestAPIWithRedisDown(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	st := f.showtime(t, f.dune, f.hall, base)
	seats := seatIDsByLabel(t, f.pool, f.hall.ID)
	redis := newDeadRedisEnv(t)
	api := newAPIServerWith(t, f, apiOptions{
		redis:  redis,
		limits: config.RateLimitConfig{BookingPerMin: 1, AuthIPPerMin: 1, LoginEmailPerMin: 1},
	})

	ann, bob := api.signUp("ann@example.com"), api.signUp("bob@example.com") // four auth attempts under a limit of 1
	a1 := map[string]any{"showtime_id": st.ID, "seat_ids": []int64{seats["A1"]}}

	if statuses := api.seatStatuses(st.ID); statuses[float64(seats["A1"])] != "available" {
		t.Fatalf("seat map = %v", statuses)
	}
	created := api.doWith(http.MethodPost, "/v1/bookings", ann, a1, withKey("book-1"))
	if created.status != http.StatusCreated {
		t.Fatalf("booking = %d %v", created.status, created.body)
	}
	if statuses := api.seatStatuses(st.ID); statuses[float64(seats["A1"])] != "held" {
		t.Errorf("seat map after the booking = %v", statuses)
	}
	if r := api.do(http.MethodPost, "/v1/bookings", bob, a1); r.status != http.StatusConflict || r.code() != "SEAT_UNAVAILABLE" {
		t.Errorf("competing booking = %d %v", r.status, r.body)
	}
	// Without Redis, a retry is not replayed: it runs again, and the database refuses a duplicate.
	if r := api.doWith(http.MethodPost, "/v1/bookings", ann, a1, withKey("book-1")); r.status != http.StatusConflict {
		t.Errorf("retry without Redis = %d %v, want a conflict from the database", r.status, r.body)
	}

	location := created.header.Get("Location")
	paid := api.doWith(http.MethodPost, location+"/payments", ann,
		map[string]string{"payment_method": "local", "payment_token": "tok_success"}, withKey("pay-1"))
	if paid.status != http.StatusOK {
		t.Fatalf("payment = %d %v", paid.status, paid.body)
	}
	if statuses := api.seatStatuses(st.ID); statuses[float64(seats["A1"])] != "sold" {
		t.Errorf("seat map after the payment = %v", statuses)
	}

	for _, op := range []string{
		metrics.OpHoldAcquire, metrics.OpCacheGet, metrics.OpCacheSet, metrics.OpCacheInvalidate,
		metrics.OpIdempotency, metrics.OpRateLimit,
	} {
		if n := redis.failedOpen(op); n == 0 {
			t.Errorf("no %s failure was counted", op)
		}
	}
}
