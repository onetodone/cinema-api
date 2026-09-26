package redis

import (
	"context"
	"fmt"
	"time"

	"github.com/onetodone/cinema-api/internal/platform/metrics"
)

var rateLimit = script("rate_limit.lua")

// RateLimiter implements middleware.RateLimiter with fixed windows: the first attempt of a key opens a window of
// the configured length, and at most limit attempts pass within it. A client can therefore make up to twice the
// limit in a short burst across the end of one window and the start of the next, which is precise enough to stop
// floods and password guessing, and costs one round trip per attempt.
//
// Keys are named <prefix>:rl:<name>:<key>.
type RateLimiter struct {
	s      *Store
	name   string
	limit  int
	window time.Duration
}

// RateLimiter returns a limiter that lets limit attempts per key through in each window. name separates its
// keys from those of other limiters.
func (s *Store) RateLimiter(name string, limit int, window time.Duration) *RateLimiter {
	if limit < 1 || window < time.Millisecond {
		panic(fmt.Sprintf("redis: rate limit %q needs a positive limit and window, got %d per %s", name, limit, window))
	}
	return &RateLimiter{s: s, name: name, limit: limit, window: window}
}

// Allow counts an attempt of key and reports whether it is within the limit. When it is not, retryAfter is the
// time until the window ends.
func (l *RateLimiter) Allow(ctx context.Context, key string) (bool, time.Duration, error) {
	res, err := rateLimit.Run(ctx, l.s.rdb, []string{l.s.key("rl", l.name, key)}, l.window.Milliseconds()).Int64Slice()
	if err == nil && len(res) != 2 {
		err = fmt.Errorf("unexpected reply %v", res)
	}
	if err != nil {
		l.s.failed(ctx, metrics.OpRateLimit, err)
		return false, 0, fmt.Errorf("count an attempt against rate limit %s: %w", l.name, err)
	}
	attempts, ttl := res[0], time.Duration(res[1])*time.Millisecond
	if attempts <= int64(l.limit) {
		return true, 0, nil
	}
	return false, max(ttl, time.Millisecond), nil
}
