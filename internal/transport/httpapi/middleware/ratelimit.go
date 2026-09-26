package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/principal"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

// maxPeekBytes is how much of a request body a middleware reads to look into it. It matches the body limit of
// the handlers, which reject a larger body with 413 anyway.
const maxPeekBytes = 64 << 10

// RateLimiter counts attempts per key and tells whether one more is within the limit. It is implemented by
// repository/redis.RateLimiter.
type RateLimiter interface {
	// Allow counts an attempt of key. When it is over the limit, retryAfter is the time until the limit resets.
	Allow(ctx context.Context, key string) (allowed bool, retryAfter time.Duration, err error)
}

// RateKey returns the key that a request counts against. ok false lets the request through uncounted.
type RateKey func(r *http.Request) (key string, ok bool)

// RateLimit answers 429 RATE_LIMITED, with Retry-After, to requests over the limit of their key, and counts them
// in cinema_rate_limit_rejections_total under name. When the limiter fails, the request passes: a limit protects
// capacity and slows attackers down, but correctness never depends on it.
func RateLimit(name string, limiter RateLimiter, key RateKey, m *metrics.Metrics) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k, ok := key(r)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			allowed, retryAfter, err := limiter.Allow(r.Context(), k)
			if err != nil || allowed {
				next.ServeHTTP(w, r) // on error: fail open, the limiter recorded the failure
				return
			}
			m.RateLimitRejections.WithLabelValues(name).Inc()
			problem.Write(w, r, problem.TooManyRequests(retryAfter))
		})
	}
}

// ByUser counts requests per authenticated caller. It must run inside Authenticate; without a caller the
// request passes uncounted, and the handler fails closed.
func ByUser(r *http.Request) (string, bool) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		return "", false
	}
	return p.UserID.String(), true
}

// ByClientIP counts requests per client address, as ClientIP finds it. IPv6 clients count per /64 network,
// because one subscriber usually gets a whole /64 and could otherwise pick a new address for every attempt.
func ByClientIP(trusted []netip.Prefix) RateKey {
	return func(r *http.Request) (string, bool) {
		addr, ok := ClientIP(r, trusted)
		if !ok {
			return "", false
		}
		if addr.Is6() {
			return netip.PrefixFrom(addr, 64).Masked().String(), true
		}
		return addr.String(), true
	}
}

// ByRefreshSession counts requests per session, as named by the refresh token in the cookie. A request whose cookie
// holds no well-formed token counts per client address instead, as ByClientIP does, so that it cannot dodge the
// limit by sending garbage. Counting well-formed tokens per session keeps the users behind one NAT from sharing a
// budget; a refresh with a made-up session id costs one lookup by primary key and no password hash.
func ByRefreshSession(cookie string, trusted []netip.Prefix) RateKey {
	byIP := ByClientIP(trusted)
	return func(r *http.Request) (string, bool) {
		if c, err := r.Cookie(cookie); err == nil {
			if token, ok := domain.ParseRefreshToken(c.Value); ok {
				return token.SessionID.String(), true
			}
		}
		key, ok := byIP(r)
		return "ip:" + key, ok
	}
}

// ByEmail counts requests per the "email" field of their JSON body, trimmed and lowercased like the unique index
// on account emails. The key is a hash of the address, so no address is stored in Redis. A body without a
// readable email passes uncounted: the handler rejects it without checking a password.
func ByEmail(r *http.Request) (string, bool) {
	body, ok := peekBody(r)
	if !ok {
		return "", false
	}
	var creds struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &creds); err != nil {
		return "", false
	}
	email := strings.ToLower(strings.TrimSpace(creds.Email))
	if email == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(email))
	return hex.EncodeToString(sum[:16]), true
}

// ClientIP returns the address of the client that sent r. A request that arrives from a trusted proxy is
// attributed to the address the proxies recorded in X-Forwarded-For: the rightmost one that is not a trusted proxy
// itself. The addresses to the left of it were written by the client, which can put anything there. ok is false
// when r.RemoteAddr is not an IP address.
func ClientIP(r *http.Request, trusted []netip.Prefix) (netip.Addr, bool) {
	addr, err := parseAddr(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	if !isTrusted(addr, trusted) {
		return addr, true
	}

	var hops []string
	for _, line := range r.Header.Values("X-Forwarded-For") {
		for hop := range strings.SplitSeq(line, ",") {
			hops = append(hops, strings.TrimSpace(hop))
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := parseAddr(hops[i])
		if err != nil {
			// A trusted proxy wrote something that is not an address. Attribute the request to that proxy
			// rather than trust anything further left.
			return addr, true
		}
		addr = hop
		if !isTrusted(addr, trusted) {
			return addr, true
		}
	}
	return addr, true // every hop is a trusted proxy: the leftmost one is where the request came from
}

// parseAddr parses an IP address with or without a port, such as "192.0.2.1", "192.0.2.1:443", or "[2001:db8::1]:443".
func parseAddr(s string) (netip.Addr, error) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap(), nil
	}
	addr, err := netip.ParseAddr(s)
	return addr.Unmap(), err
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// peekBody reads the request body, up to maxPeekBytes, and puts it back, so that the handler reads it again from
// the start. ok is false when the body is larger or cannot be read; the handler then gets exactly what the client
// sent, and answers for it.
func peekBody(r *http.Request) ([]byte, bool) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, true
	}
	head, err := io.ReadAll(io.LimitReader(r.Body, maxPeekBytes+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
	if err != nil || len(head) > maxPeekBytes {
		return nil, false
	}
	return head, true
}
