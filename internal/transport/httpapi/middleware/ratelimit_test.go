package middleware

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/principal"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

func newMetrics() *metrics.Metrics {
	return metrics.New(prometheus.NewRegistry())
}

// scriptedLimiter answers every attempt the same way and records the keys.
type scriptedLimiter struct {
	allowed    bool
	retryAfter time.Duration
	err        error
	keys       []string
}

func (l *scriptedLimiter) Allow(_ context.Context, key string) (bool, time.Duration, error) {
	l.keys = append(l.keys, key)
	return l.allowed, l.retryAfter, l.err
}

func okHandler(called *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*called = true
		w.WriteHeader(http.StatusNoContent)
	})
}

func TestRateLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		limiter    *scriptedLimiter
		key        RateKey
		wantStatus int
		wantRetry  string
	}{
		{
			name:       "within the limit",
			limiter:    &scriptedLimiter{allowed: true},
			key:        func(*http.Request) (string, bool) { return "k", true },
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "over the limit",
			limiter:    &scriptedLimiter{retryAfter: 1500 * time.Millisecond},
			key:        func(*http.Request) (string, bool) { return "k", true },
			wantStatus: http.StatusTooManyRequests,
			wantRetry:  "2", // rounded up
		},
		{
			name:       "limiter fails open",
			limiter:    &scriptedLimiter{err: errors.New("redis down")},
			key:        func(*http.Request) (string, bool) { return "k", true },
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "request without a key passes uncounted",
			limiter:    &scriptedLimiter{},
			key:        func(*http.Request) (string, bool) { return "", false },
			wantStatus: http.StatusNoContent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newMetrics()
			var called bool
			h := RateLimit(metrics.LimitBooking, tt.limiter, tt.key, m)(okHandler(&called))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/bookings", nil))

			if rec.Code != tt.wantStatus || rec.Header().Get("Retry-After") != tt.wantRetry {
				t.Errorf("status = %d, Retry-After %q; want %d, %q", rec.Code, rec.Header().Get("Retry-After"), tt.wantStatus, tt.wantRetry)
			}
			if called != (tt.wantStatus == http.StatusNoContent) {
				t.Errorf("handler called = %t", called)
			}
			rejected := testutil.ToFloat64(m.RateLimitRejections.WithLabelValues(metrics.LimitBooking))
			if want := map[bool]float64{true: 1, false: 0}[tt.wantStatus == http.StatusTooManyRequests]; rejected != want {
				t.Errorf("rejections = %v, want %v", rejected, want)
			}
			if rec.Code == http.StatusTooManyRequests && !strings.Contains(rec.Body.String(), problem.CodeRateLimited) {
				t.Errorf("body = %s, want code %s", rec.Body, problem.CodeRateLimited)
			}
		})
	}
}

func TestClientIP(t *testing.T) {
	t.Parallel()

	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("2001:db8:ffff::/48")}
	tests := []struct {
		name   string
		remote string
		xff    []string
		want   string
	}{
		{name: "direct client", remote: "198.51.100.7:5000", want: "198.51.100.7"},
		{name: "direct client ignores forwarded addresses", remote: "198.51.100.7:5000", xff: []string{"203.0.113.9"}, want: "198.51.100.7"},
		{name: "trusted proxy", remote: "10.0.0.5:5000", xff: []string{"203.0.113.9"}, want: "203.0.113.9"},
		{
			name: "spoofed hops left of the real client are ignored", remote: "10.0.0.5:5000",
			xff: []string{"1.1.1.1, 203.0.113.9"}, want: "203.0.113.9",
		},
		{
			name: "chain of trusted proxies over several header lines", remote: "10.0.0.5:5000",
			xff: []string{"1.1.1.1, 203.0.113.9", "10.1.1.1"}, want: "203.0.113.9",
		},
		{name: "every hop trusted", remote: "10.0.0.5:5000", xff: []string{"10.2.2.2, 10.1.1.1"}, want: "10.2.2.2"},
		{name: "trusted proxy without the header", remote: "10.0.0.5:5000", want: "10.0.0.5"},
		{name: "garbage from a trusted proxy", remote: "10.0.0.5:5000", xff: []string{"203.0.113.9, not-an-ip"}, want: "10.0.0.5"},
		{name: "hop with a port", remote: "10.0.0.5:5000", xff: []string{"203.0.113.9:4711"}, want: "203.0.113.9"},
		{name: "IPv6 through an IPv6 proxy", remote: "[2001:db8:ffff::1]:443", xff: []string{"2001:db8:1::42"}, want: "2001:db8:1::42"},
		{name: "IPv4-mapped IPv6 address", remote: "[::ffff:198.51.100.7]:5000", want: "198.51.100.7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			got, ok := ClientIP(r, trusted)
			if !ok || got.String() != tt.want {
				t.Errorf("ClientIP = %s, %t; want %s", got, ok, tt.want)
			}
		})
	}

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
	r.RemoteAddr = "pipe"
	if _, ok := ClientIP(r, trusted); ok {
		t.Error("ClientIP of a non-IP remote address reported ok")
	}
}

func TestByClientIPGroupsIPv6Networks(t *testing.T) {
	t.Parallel()

	key := ByClientIP(nil)
	keyOf := func(remote string) string {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
		r.RemoteAddr = remote
		k, ok := key(r)
		if !ok {
			t.Fatalf("no key for %s", remote)
		}
		return k
	}

	if a, b := keyOf("[2001:db8:1:2::1]:1"), keyOf("[2001:db8:1:2:ffff::9]:2"); a != b || a != "2001:db8:1:2::/64" {
		t.Errorf("keys of one /64 = %q and %q, want both 2001:db8:1:2::/64", a, b)
	}
	if k := keyOf("198.51.100.7:1"); k != "198.51.100.7" {
		t.Errorf("IPv4 key = %q", k)
	}
}

func TestByEmailHashesTheNormalizedAddressAndKeepsTheBody(t *testing.T) {
	t.Parallel()

	keyOf := func(body string) (string, bool, string) {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body))
		k, ok := ByEmail(r)
		rest, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		return k, ok, string(rest)
	}

	body := `{"email":" Ann@Example.COM ","password":"x"}`
	a, ok, rest := keyOf(body)
	if !ok || rest != body {
		t.Fatalf("key %q, %t, body afterwards %q; want a key and the untouched body", a, ok, rest)
	}
	if b, _, _ := keyOf(`{"email":"ann@example.com"}`); a != b || strings.Contains(a, "ann") {
		t.Errorf("keys %q and %q, want one opaque key for both spellings", a, b)
	}

	for _, body := range []string{``, `not json`, `{"email":"  "}`, `{"password":"x"}`} {
		if k, ok, rest := keyOf(body); ok || rest != body {
			t.Errorf("body %q: key %q, %t, body afterwards %q; want no key and the untouched body", body, k, ok, rest)
		}
	}

	large := `{"email":"ann@example.com","padding":"` + strings.Repeat("x", maxPeekBytes) + `"}`
	if _, ok, rest := keyOf(large); ok || rest != large {
		t.Errorf("large body: key found = %t, body intact = %t; want no key and the whole body", ok, rest == large)
	}
}

func TestByUser(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
	if _, ok := ByUser(r); ok {
		t.Error("ByUser found a key without a caller")
	}
	id := uuid.NewV7()
	r = r.WithContext(principal.NewContext(r.Context(), domain.Principal{UserID: id, Role: domain.RoleCustomer}))
	if k, ok := ByUser(r); !ok || k != id.String() {
		t.Errorf("ByUser = %q, %t; want %s", k, ok, id)
	}
}
