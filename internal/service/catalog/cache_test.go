package catalog

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onetodone/cinema-api/internal/domain"
)

// fakeCache is an in-memory Cache. A non-nil err makes every lookup and write fail.
type fakeCache struct {
	mu        sync.Mutex
	seatMaps  map[int64]SeatMap
	schedules map[string][]domain.Showtime
	err       error
	sets      int
}

func newFakeCache() *fakeCache {
	return &fakeCache{seatMaps: map[int64]SeatMap{}, schedules: map[string][]domain.Showtime{}}
}

func (c *fakeCache) SeatMap(_ context.Context, id int64) (SeatMap, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sm, ok := c.seatMaps[id]
	return sm, ok, c.err
}

func (c *fakeCache) SetSeatMap(_ context.Context, id int64, sm SeatMap) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sets++
	if c.err == nil {
		c.seatMaps[id] = sm
	}
	return c.err
}

func (c *fakeCache) Schedule(_ context.Context, day string) ([]domain.Showtime, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	list, ok := c.schedules[day]
	return list, ok, c.err
}

func (c *fakeCache) SetSchedule(_ context.Context, day string, list []domain.Showtime) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sets++
	if c.err == nil {
		c.schedules[day] = list
	}
	return c.err
}

// countingRepo counts seat map reads and can hold them until release is closed.
type countingRepo struct {
	fakeRepo
	reads   atomic.Int32
	release chan struct{} // nil: reads return at once
}

func (r *countingRepo) GetShowtime(ctx context.Context, id int64) (domain.Showtime, error) {
	r.reads.Add(1)
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return domain.Showtime{}, ctx.Err()
		}
	}
	return r.fakeRepo.GetShowtime(ctx, id)
}

func seatMapRepo() *countingRepo {
	return &countingRepo{fakeRepo: fakeRepo{
		showtimes: map[int64]domain.Showtime{
			5: {ID: 5, StartsAt: time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)},
		},
		seats: map[int64][]domain.ShowtimeSeat{5: {
			{SeatID: 1, Status: domain.SeatAvailable},
			{SeatID: 2, Status: domain.SeatHeld},
		}},
	}}
}

func TestSeatMapIsServedFromTheCache(t *testing.T) {
	t.Parallel()

	loc := mustLoad(t, "Asia/Dubai")
	repo, cache := seatMapRepo(), newFakeCache()
	svc := New(repo, loc, WithCache(cache))

	for i := range 3 {
		sm, err := svc.SeatMap(t.Context(), 5)
		if err != nil {
			t.Fatal(err)
		}
		if sm.Summary != (SeatSummary{Available: 1, Held: 1, Total: 2}) || len(sm.Seats) != 2 {
			t.Errorf("call %d: seat map = %+v", i, sm)
		}
		if sm.Showtime.StartsAt.Location() != loc {
			t.Errorf("call %d: start time is not localized", i)
		}
	}
	if n := repo.reads.Load(); n != 1 {
		t.Errorf("repository read %d times, want 1: the later calls hit the cache", n)
	}
	if _, ok := cache.seatMaps[5]; !ok || cache.sets != 1 {
		t.Errorf("cache holds %v after %d writes, want showtime 5 after 1 write", cache.seatMaps, cache.sets)
	}
}

func TestScheduleIsServedFromTheCache(t *testing.T) {
	t.Parallel()

	loc := mustLoad(t, "Asia/Dubai")
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	repo := &fakeRepo{showtimes: map[int64]domain.Showtime{
		1: {ID: 1, Movie: domain.MovieSummary{ID: 7}, StartsAt: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)},
		2: {ID: 2, Movie: domain.MovieSummary{ID: 8}, StartsAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)},
	}}
	cache := newFakeCache()
	svc := New(repo, loc, WithCache(cache))

	all, err := svc.Schedule(t.Context(), ScheduleQuery{Day: day})
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := svc.Schedule(t.Context(), ScheduleQuery{Day: day, MovieID: 8})
	if err != nil {
		t.Fatal(err)
	}

	if len(repo.listed) != 1 {
		t.Errorf("repository listed %d times, want 1: both filters share the cached day", len(repo.listed))
	}
	if cached := cache.schedules["2026-10-01"]; len(cached) != 2 {
		t.Errorf("cached day 2026-10-01 = %+v, want both showtimes", cached)
	}
	if len(all.Showtimes) != 2 || len(filtered.Showtimes) != 1 || filtered.Showtimes[0].ID != 2 {
		t.Errorf("all = %+v, movie 8 = %+v", all.Showtimes, filtered.Showtimes)
	}
	if filtered.Showtimes[0].StartsAt.Location() != loc {
		t.Error("start time of a cached showtime is not localized")
	}
	if cache.schedules["2026-10-01"][0].StartsAt.Location() == loc {
		t.Error("localizing changed the cached list in place")
	}
}

func TestCacheFailuresFallBackToTheRepository(t *testing.T) {
	t.Parallel()

	repo, cache := seatMapRepo(), newFakeCache()
	cache.err = errors.New("redis down")
	svc := New(repo, time.UTC, WithCache(cache))

	for range 2 {
		sm, err := svc.SeatMap(t.Context(), 5)
		if err != nil || sm.Summary.Total != 2 {
			t.Fatalf("seat map = %+v, %v; want it from the repository", sm, err)
		}
	}
	if n := repo.reads.Load(); n != 2 {
		t.Errorf("repository read %d times, want 2", n)
	}
	if _, err := svc.Schedule(t.Context(), ScheduleQuery{}); err != nil {
		t.Errorf("schedule with a failing cache: %v", err)
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	t.Parallel()

	repo, cache := seatMapRepo(), newFakeCache()
	svc := New(repo, time.UTC, WithCache(cache))

	for range 2 {
		if _, err := svc.SeatMap(t.Context(), 99); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	}
	if cache.sets != 0 || repo.reads.Load() != 2 {
		t.Errorf("%d cache writes and %d reads, want none and 2", cache.sets, repo.reads.Load())
	}
}

func TestConcurrentMissesShareOneRead(t *testing.T) {
	t.Parallel()

	repo := seatMapRepo()
	repo.release = make(chan struct{})
	svc := New(repo, time.UTC, WithCache(newFakeCache()))

	const callers = 50
	var (
		wg     sync.WaitGroup
		failed atomic.Int32
	)
	for range callers {
		wg.Go(func() {
			if sm, err := svc.SeatMap(t.Context(), 5); err != nil || sm.Summary.Total != 2 {
				failed.Add(1)
			}
		})
	}
	// Let every caller join the flight before the read returns.
	for repo.reads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(repo.release)
	wg.Wait()

	if n := repo.reads.Load(); n != 1 {
		t.Errorf("repository read %d times for %d concurrent callers, want 1", n, callers)
	}
	if n := failed.Load(); n != 0 {
		t.Errorf("%d callers got no seat map", n)
	}
}

func TestSharedReadOutlivesACanceledCaller(t *testing.T) {
	t.Parallel()

	repo := seatMapRepo()
	repo.release = make(chan struct{})
	svc := New(repo, time.UTC)

	ctx, cancel := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() {
		_, err := svc.SeatMap(ctx, 5)
		first <- err
	}()
	for repo.reads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	second := make(chan error, 1)
	go func() {
		sm, err := svc.SeatMap(t.Context(), 5)
		if err == nil && sm.Summary.Total != 2 {
			err = errors.New("wrong seat map")
		}
		second <- err
	}()
	time.Sleep(20 * time.Millisecond) // the second caller joins the flight of the first

	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Errorf("canceled caller got %v, want context.Canceled", err)
	}
	close(repo.release)
	if err := <-second; err != nil {
		t.Errorf("second caller: %v; the read must survive the first caller's cancellation", err)
	}
	if n := repo.reads.Load(); n != 1 {
		t.Errorf("repository read %d times, want 1", n)
	}
}
