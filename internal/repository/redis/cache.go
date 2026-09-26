package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/service/catalog"
)

// CatalogCache implements catalog.Cache and booking.SeatMapCache. It keeps seat maps and day schedules as JSON
// for a short TTL. The booking service deletes the seat map of a showtime after every committed seat change;
// schedules only expire. A TTL of 0 turns that cache off: lookups miss without asking Redis.
//
// Keys are named <prefix>:seatmap:{<showtimeID>} and <prefix>:schedule:<yyyy-mm-dd>, the day in the cinema's
// time zone.
type CatalogCache struct {
	s           *Store
	seatMapTTL  time.Duration
	scheduleTTL time.Duration
}

// CatalogCache returns the catalog cache with the given TTLs.
func (s *Store) CatalogCache(seatMapTTL, scheduleTTL time.Duration) *CatalogCache {
	return &CatalogCache{s: s, seatMapTTL: seatMapTTL, scheduleTTL: scheduleTTL}
}

func (c *CatalogCache) seatMapKey(showtimeID int64) string {
	return c.s.key("seatmap", "{"+strconv.FormatInt(showtimeID, 10)+"}")
}

func (c *CatalogCache) scheduleKey(day string) string {
	return c.s.key("schedule", day)
}

// SeatMap returns the cached seat map of a showtime.
func (c *CatalogCache) SeatMap(ctx context.Context, showtimeID int64) (catalog.SeatMap, bool, error) {
	var sm catalog.SeatMap
	ok, err := c.get(ctx, metrics.CacheSeatMap, c.seatMapKey(showtimeID), c.seatMapTTL, &sm)
	return sm, ok, err
}

// SetSeatMap caches the seat map of a showtime.
func (c *CatalogCache) SetSeatMap(ctx context.Context, showtimeID int64, sm catalog.SeatMap) error {
	return c.set(ctx, c.seatMapKey(showtimeID), c.seatMapTTL, sm)
}

// Schedule returns the cached showtimes of a day.
func (c *CatalogCache) Schedule(ctx context.Context, day string) ([]domain.Showtime, bool, error) {
	var showtimes []domain.Showtime
	ok, err := c.get(ctx, metrics.CacheSchedule, c.scheduleKey(day), c.scheduleTTL, &showtimes)
	return showtimes, ok, err
}

// SetSchedule caches the showtimes of a day.
func (c *CatalogCache) SetSchedule(ctx context.Context, day string, showtimes []domain.Showtime) error {
	return c.set(ctx, c.scheduleKey(day), c.scheduleTTL, showtimes)
}

// InvalidateSeatMaps deletes the cached seat maps of the given showtimes.
func (c *CatalogCache) InvalidateSeatMaps(ctx context.Context, showtimeIDs ...int64) error {
	if c.seatMapTTL <= 0 || len(showtimeIDs) == 0 {
		return nil
	}
	// One DEL per key, not one DEL of all keys: in Redis Cluster the keys live in different slots. The pipeline
	// still sends them in one round trip.
	_, err := c.s.rdb.Pipelined(ctx, func(pipe goredis.Pipeliner) error {
		for _, id := range showtimeIDs {
			pipe.Del(ctx, c.seatMapKey(id))
		}
		return nil
	})
	if err != nil {
		c.s.failed(ctx, metrics.OpCacheInvalidate, err)
		return fmt.Errorf("invalidate the seat maps of showtimes %v: %w", showtimeIDs, err)
	}
	return nil
}

// get decodes the value under key into dst and reports whether there was one.
func (c *CatalogCache) get(ctx context.Context, cache, key string, ttl time.Duration, dst any) (bool, error) {
	if ttl <= 0 {
		return false, nil
	}
	raw, err := c.s.rdb.Get(ctx, key).Bytes()
	switch {
	case errors.Is(err, goredis.Nil):
		c.s.metrics.CacheRequests.WithLabelValues(cache, metrics.CacheMiss).Inc()
		return false, nil
	case err != nil:
		c.s.metrics.CacheRequests.WithLabelValues(cache, metrics.CacheError).Inc()
		c.s.failed(ctx, metrics.OpCacheGet, err)
		return false, fmt.Errorf("read %s: %w", key, err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		// An entry written by an older release, for example. The next set replaces it.
		c.s.metrics.CacheRequests.WithLabelValues(cache, metrics.CacheError).Inc()
		c.s.logger.WarnContext(ctx, "undecodable cache entry; reading from the database",
			slog.String("key", key), slog.Any("error", err))
		return false, nil
	}
	c.s.metrics.CacheRequests.WithLabelValues(cache, metrics.CacheHit).Inc()
	return true, nil
}

// set stores v as JSON under key for ttl.
func (c *CatalogCache) set(ctx context.Context, key string, ttl time.Duration, v any) error {
	if ttl <= 0 {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}
	if err := c.s.rdb.Set(ctx, key, raw, ttl).Err(); err != nil {
		c.s.failed(ctx, metrics.OpCacheSet, err)
		return fmt.Errorf("write %s: %w", key, err)
	}
	return nil
}
