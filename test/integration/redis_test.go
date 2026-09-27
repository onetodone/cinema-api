//go:build integration

package integration

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	goredis "github.com/redis/go-redis/v9"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/service/catalog"
)

// pttl returns the remaining lifetime of key, or a negative duration if it does not exist.
func pttl(t *testing.T, env *redisEnv, key string) time.Duration {
	t.Helper()
	d, err := env.client.PTTL(t.Context(), key).Result()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestHoldGate(t *testing.T) {
	t.Parallel()
	env := newRedisEnv(t)
	gate := env.store.HoldGate()
	ctx := t.Context()
	ann, bob := uuid.NewV7(), uuid.NewV7()
	key := func(seat string) string { return env.prefix + ":hold:{7}:" + seat }

	if conflicts, err := gate.Acquire(ctx, 7, []int64{1, 2}, ann, 15*time.Second); err != nil || len(conflicts) != 0 {
		t.Fatalf("first claim = %v, %v", conflicts, err)
	}
	if ttl := pttl(t, env, key("1")); ttl <= 14*time.Second || ttl > 15*time.Second {
		t.Errorf("claim TTL = %s, want about 15s", ttl)
	}
	if owner := env.client.Get(ctx, key("2")).Val(); owner != ann.String() {
		t.Errorf("seat 2 is claimed by %q, want the token %s", owner, ann)
	}

	// All or nothing: seat 3 is free, but seat 2 is not, so bob claims neither.
	conflicts, err := gate.Acquire(ctx, 7, []int64{2, 3}, bob, 15*time.Second)
	if err != nil || !slices.Equal(conflicts, []int64{2}) {
		t.Fatalf("competing claim = %v, %v; want conflict on seat 2", conflicts, err)
	}
	if n := env.client.Exists(ctx, key("3")).Val(); n != 0 {
		t.Error("a rejected claim left seat 3 claimed")
	}
	if n := env.gateRejections(); n != 1 {
		t.Errorf("gate rejections = %v, want 1", n)
	}
	// The same token may claim its seats again, and the seats of other showtimes are separate.
	if conflicts, err := gate.Acquire(ctx, 7, []int64{1, 2}, ann, time.Minute); err != nil || len(conflicts) != 0 {
		t.Errorf("repeated claim = %v, %v", conflicts, err)
	}
	if conflicts, err := gate.Acquire(ctx, 8, []int64{1, 2}, bob, time.Minute); err != nil || len(conflicts) != 0 {
		t.Errorf("claim on another showtime = %v, %v", conflicts, err)
	}

	// Extend and release change only the keys of their own token.
	until := time.Now().Add(time.Hour)
	if err := gate.ExtendUntil(ctx, 7, []int64{1, 2}, bob, until); err != nil {
		t.Fatal(err)
	}
	if ttl := pttl(t, env, key("1")); ttl > time.Minute {
		t.Errorf("another token extended the claim to %s", ttl)
	}
	if err := gate.ExtendUntil(ctx, 7, []int64{1, 2}, ann, until); err != nil {
		t.Fatal(err)
	}
	if ttl := pttl(t, env, key("1")); ttl < 59*time.Minute {
		t.Errorf("extended TTL = %s, want about 1h", ttl)
	}
	if err := gate.Release(ctx, 7, []int64{1, 2}, bob); err != nil {
		t.Fatal(err)
	}
	if n := env.client.Exists(ctx, key("1"), key("2")).Val(); n != 2 {
		t.Errorf("another token released %d keys", 2-n)
	}
	if err := gate.Release(ctx, 7, []int64{1}, ann); err != nil {
		t.Fatal(err)
	}
	if n := env.client.Exists(ctx, key("1")).Val(); n != 0 {
		t.Error("seat 1 is still claimed after its release")
	}
	// A deadline in the past ends the claim at once.
	if err := gate.ExtendUntil(ctx, 7, []int64{2}, ann, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if n := env.client.Exists(ctx, key("2")).Val(); n != 0 {
		t.Error("seat 2 is still claimed after its deadline")
	}
}

func TestCatalogCache(t *testing.T) {
	t.Parallel()
	env := newRedisEnv(t)
	cache := env.store.CatalogCache(time.Minute, 30*time.Second)
	ctx := t.Context()

	if _, ok, err := cache.SeatMap(ctx, 5); ok || err != nil {
		t.Fatalf("empty cache lookup = %t, %v", ok, err)
	}
	loc := time.FixedZone("GST", 4*3600)
	want := catalog.SeatMap{
		Showtime: domain.Showtime{
			ID: 5, Movie: domain.MovieSummary{ID: 1, Title: "Dune", Genres: []domain.Genre{domain.GenreScienceFiction}},
			Hall: domain.Hall{ID: 2, Name: "Hall 1"}, StartsAt: time.Date(2030, 1, 1, 19, 0, 0, 0, loc),
			Language:       domain.LanguageVersion{Audio: "eng", Subtitles: "tha"},
			BasePriceCents: 900, Status: domain.ShowtimeScheduled,
		},
		Seats:   []domain.ShowtimeSeat{{SeatID: 1, Row: "A", Number: 1, Type: domain.SeatVIP, PriceCents: 1350, Status: domain.SeatHeld}},
		Summary: catalog.SeatSummary{Held: 1, Total: 1},
	}
	if err := cache.SetSeatMap(ctx, 5, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := cache.SeatMap(ctx, 5)
	if err != nil || !ok || !got.Showtime.StartsAt.Equal(want.Showtime.StartsAt) ||
		!reflect.DeepEqual(got.Showtime.Movie, want.Showtime.Movie) || got.Showtime.Language != want.Showtime.Language ||
		!slices.Equal(got.Seats, want.Seats) || got.Summary != want.Summary {
		t.Fatalf("cached seat map = %+v, %t, %v; want %+v", got, ok, err, want)
	}
	if ttl := pttl(t, env, env.prefix+":seatmap:v2:{5}"); ttl <= 59*time.Second {
		t.Errorf("seat map TTL = %s, want about 1m", ttl)
	}

	if err := cache.SetSchedule(ctx, "2030-01-01", []domain.Showtime{want.Showtime}); err != nil {
		t.Fatal(err)
	}
	if list, ok, err := cache.Schedule(ctx, "2030-01-01"); err != nil || !ok || len(list) != 1 || list[0].ID != 5 {
		t.Errorf("cached schedule = %+v, %t, %v", list, ok, err)
	}
	if ttl := pttl(t, env, env.prefix+":schedule:v2:2030-01-01"); ttl <= 29*time.Second || ttl > 30*time.Second {
		t.Errorf("schedule TTL = %s, want about 30s", ttl)
	}

	if err := cache.InvalidateSeatMaps(ctx, 5, 6); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := cache.SeatMap(ctx, 5); ok {
		t.Error("the seat map is still cached after its invalidation")
	}
	if err := cache.InvalidateSchedule(ctx, "2030-01-01"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := cache.Schedule(ctx, "2030-01-01"); ok {
		t.Error("the schedule is still cached after its invalidation")
	}
	if hits, misses := env.cacheRequests(metrics.CacheSeatMap, metrics.CacheHit), env.cacheRequests(metrics.CacheSeatMap, metrics.CacheMiss); hits != 1 || misses != 2 {
		t.Errorf("seat map hits/misses = %v/%v, want 1/2", hits, misses)
	}

	// An entry that does not decode, such as one written by an older release, is a miss.
	if err := env.client.Set(ctx, env.prefix+":seatmap:v2:{9}", "{not json", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cache.SeatMap(ctx, 9); ok || err != nil {
		t.Errorf("undecodable entry = %t, %v; want a miss", ok, err)
	}

	// A TTL of 0 turns a cache off: nothing is written or read.
	off := env.store.CatalogCache(0, 0)
	if err := off.SetSeatMap(ctx, 10, want); err != nil {
		t.Fatal(err)
	}
	if n := env.client.Exists(ctx, env.prefix+":seatmap:v2:{10}").Val(); n != 0 {
		t.Error("a cache with TTL 0 wrote an entry")
	}
}

func TestIdempotencyStore(t *testing.T) {
	t.Parallel()
	env := newRedisEnv(t)
	store := env.store.Idempotency()
	ctx := t.Context()
	key := env.prefix + ":idem:user:k1"

	if existing, claimed, err := store.Claim(ctx, "user:k1", "first", []byte("pending"), time.Minute); err != nil || !claimed || existing != nil {
		t.Fatalf("first claim = %q, %t, %v", existing, claimed, err)
	}
	if ttl := pttl(t, env, key); ttl <= 59*time.Second {
		t.Errorf("claim TTL = %s, want about 1m", ttl)
	}
	if existing, claimed, err := store.Claim(ctx, "user:k1", "second", []byte("other"), time.Minute); err != nil || claimed || string(existing) != "pending" {
		t.Errorf("second claim = %q, %t, %v; want the first record", existing, claimed, err)
	}

	if done, err := store.Complete(ctx, "user:k1", "second", []byte("hijacked"), time.Hour); err != nil || done {
		t.Errorf("Complete by a non-owner = %t, %v", done, err)
	}
	if err := store.Release(ctx, "user:k1", "second"); err != nil {
		t.Fatal(err)
	}
	if done, err := store.Complete(ctx, "user:k1", "first", []byte("response"), 24*time.Hour); err != nil || !done {
		t.Fatalf("Complete by the owner = %t, %v", done, err)
	}
	if existing, _, _ := store.Claim(ctx, "user:k1", "third", nil, time.Minute); string(existing) != "response" {
		t.Errorf("record after completion = %q, want the response", existing)
	}
	if ttl := pttl(t, env, key); ttl < 23*time.Hour {
		t.Errorf("completed TTL = %s, want about 24h", ttl)
	}

	if err := store.Release(ctx, "user:k1", "first"); err != nil {
		t.Fatal(err)
	}
	if n := env.client.Exists(ctx, key).Val(); n != 0 {
		t.Error("the owner's release left the record behind")
	}
}

func TestRateLimiter(t *testing.T) {
	t.Parallel()
	env := newRedisEnv(t)
	limiter := env.store.RateLimiter("test", 3, 400*time.Millisecond)
	ctx := t.Context()

	for i := range 3 {
		if ok, _, err := limiter.Allow(ctx, "ann"); !ok || err != nil {
			t.Fatalf("attempt %d = %t, %v; want allowed", i+1, ok, err)
		}
	}
	ok, retryAfter, err := limiter.Allow(ctx, "ann")
	if ok || err != nil || retryAfter <= 0 || retryAfter > 400*time.Millisecond {
		t.Fatalf("fourth attempt = %t, retry after %s, %v; want denied within the window", ok, retryAfter, err)
	}
	if ok, _, _ := limiter.Allow(ctx, "bob"); !ok {
		t.Error("another key shares the limit")
	}

	time.Sleep(retryAfter + 50*time.Millisecond)
	if ok, _, err := limiter.Allow(ctx, "ann"); !ok || err != nil {
		t.Errorf("attempt in the next window = %t, %v; want allowed", ok, err)
	}
	if ttl := pttl(t, env, env.prefix+":rl:test:ann"); ttl <= 0 || ttl > 400*time.Millisecond {
		t.Errorf("window TTL = %s, want a new window of at most 400ms", ttl)
	}
}

// TestRedisOutageFailsOpenFast checks every adapter against an address where nothing listens: each call fails at
// once, counts in the fail-open metric, and a warning is logged once per operation, not once per call.
func TestRedisOutageFailsOpenFast(t *testing.T) {
	t.Parallel()
	env := newDeadRedisEnv(t)
	ctx := t.Context()
	gate, cache := env.store.HoldGate(), env.store.CatalogCache(time.Minute, time.Minute)
	idem, limiter := env.store.Idempotency(), env.store.RateLimiter("test", 3, time.Minute)

	calls := map[string]func() error{
		metrics.OpHoldAcquire: func() error {
			_, err := gate.Acquire(ctx, 1, []int64{1}, uuid.NewV7(), time.Second)
			return err
		},
		metrics.OpHoldExtend:  func() error { return gate.ExtendUntil(ctx, 1, []int64{1}, uuid.NewV7(), time.Now()) },
		metrics.OpHoldRelease: func() error { return gate.Release(ctx, 1, []int64{1}, uuid.NewV7()) },
		metrics.OpCacheGet: func() error {
			_, _, err := cache.SeatMap(ctx, 1)
			return err
		},
		metrics.OpCacheSet:        func() error { return cache.SetSeatMap(ctx, 1, catalog.SeatMap{}) },
		metrics.OpCacheInvalidate: func() error { return cache.InvalidateSeatMaps(ctx, 1) },
		metrics.OpIdempotency: func() error {
			_, _, err := idem.Claim(ctx, "k", "owner", nil, time.Minute)
			return err
		},
		metrics.OpRateLimit: func() error {
			_, _, err := limiter.Allow(ctx, "k")
			return err
		},
	}
	for op, call := range calls {
		for range 3 {
			start := time.Now()
			err := call()
			if err == nil || errors.Is(err, goredis.Nil) {
				t.Errorf("%s against a dead Redis = %v, want an error", op, err)
			}
			if d := time.Since(start); d > 200*time.Millisecond {
				t.Errorf("%s took %s against a dead Redis, want it to fail fast", op, d)
			}
		}
		if n := env.failedOpen(op); n != 3 {
			t.Errorf("fail-open count of %s = %v, want 3", op, n)
		}
	}

	warnings := env.logs.warned()
	if len(warnings) != len(calls) {
		t.Errorf("%d warnings for %d operations, want one each:\n%s", len(warnings), len(calls), strings.Join(warnings, "\n"))
	}
}
