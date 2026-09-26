//go:build integration

package integration

import (
	"fmt"
	"testing"
	"time"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/service/catalog"
)

// TestSeatMapCacheFollowsSeatChanges caches seat maps for a whole minute, so a reader sees a change in time only
// because every committed seat change deletes the cached seat map: booking, cancel, expiry, and sale.
func TestSeatMapCacheFollowsSeatChanges(t *testing.T) {
	t.Parallel()
	redis := newRedisEnv(t)
	env := newBookingEnv(t, 3*time.Second, withRedis(redis)...)
	cat := catalog.New(env.catalog, time.UTC, catalog.WithCache(redis.store.CatalogCache(testSeatMapTTL, testScheduleTTL)))
	ctx := t.Context()
	a1 := env.seats["A1"]

	seatA1 := func() domain.SeatStatus {
		t.Helper()
		sm, err := cat.SeatMap(ctx, env.st.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sm.Seats {
			if s.SeatID == a1 {
				return s.Status
			}
		}
		t.Fatal("seat A1 is not on the seat map")
		return ""
	}
	expect := func(step string, want domain.SeatStatus) {
		t.Helper()
		for range 2 { // the second read comes from the cache
			if got := seatA1(); got != want {
				t.Errorf("%s: seat A1 is %s, want %s", step, got, want)
			}
		}
	}
	users := newUsers(t, env.pool, 3)
	book := func() domain.Booking {
		t.Helper()
		b, err := env.svc.Create(ctx, users[0], domain.NewBooking{ShowtimeID: env.st.ID, SeatIDs: []int64{a1}})
		if err != nil {
			t.Fatal(err)
		}
		users = users[1:]
		return b
	}

	expect("before any booking", domain.SeatAvailable)

	b := book()
	expect("after the booking", domain.SeatHeld)

	if err := env.svc.Cancel(ctx, b.UserID, b.ID); err != nil {
		t.Fatal(err)
	}
	expect("after the cancel", domain.SeatAvailable)

	b = book()
	backdate(t, env.pool, time.Second, b.ID)
	if batch, err := env.svc.ExpireBatch(ctx, 10); err != nil || len(batch.BookingIDs) != 1 {
		t.Fatalf("ExpireBatch = %+v, %v", batch, err)
	}
	expect("after the expiry", domain.SeatAvailable)

	b = book()
	if _, err := env.svc.Pay(ctx, b.UserID, b.ID, withLocal("tok_success")); err != nil {
		t.Fatal(err)
	}
	expect("after the sale", domain.SeatSold)

	// Five states, each read once from the database and once from the cache.
	hits, misses := redis.cacheRequests(metrics.CacheSeatMap, metrics.CacheHit), redis.cacheRequests(metrics.CacheSeatMap, metrics.CacheMiss)
	if hits != 5 || misses != 5 {
		t.Errorf("seat map cache hits/misses = %v/%v, want 5/5", hits, misses)
	}

	// The sold seat stays claimed in the gate until the showtime starts, when booking ends anyway.
	ttl := pttl(t, redis, fmt.Sprintf("%s:hold:{%d}:%d", redis.prefix, env.st.ID, a1))
	if untilStart := time.Until(env.st.StartsAt); ttl < untilStart-time.Minute || ttl > untilStart+time.Second {
		t.Errorf("gate claim of the sold seat lives %s, want about %s, until the showtime starts", ttl, untilStart)
	}
}
