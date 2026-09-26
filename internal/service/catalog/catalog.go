// Package catalog implements the read-only browsing use cases: movies, the schedule, and seat maps.
//
// The schedule and seat maps are the hot read paths, so they are served cache-aside: from the Cache when it has
// them, otherwise from the Repository, which then fills the Cache. Concurrent misses for the same data share one
// repository read (singleflight), so an expired entry of a popular seat map costs one query per process, not one
// per request. A cache that fails is treated as empty.
package catalog

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/onetodone/cinema-api/internal/domain"
)

// Paging limits for movie lists.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

const (
	// upcomingWindow and upcomingLimit bound the showtimes listed on a movie page.
	upcomingWindow = 14 * 24 * time.Hour
	upcomingLimit  = 50
)

// loadTimeout bounds a repository read that several callers share. It runs on its own context, because no single
// caller may cancel it for the others.
const loadTimeout = 10 * time.Second

// Repository is the storage the catalog needs. It is implemented by repository/postgres.Catalog.
type Repository interface {
	ListMovies(ctx context.Context, afterID int64, limit int) ([]domain.Movie, error)
	GetMovie(ctx context.Context, id int64) (domain.Movie, error)
	ListShowtimes(ctx context.Context, f domain.ShowtimeFilter) ([]domain.Showtime, error)
	GetShowtime(ctx context.Context, id int64) (domain.Showtime, error)
	ListShowtimeSeats(ctx context.Context, showtimeID int64) ([]domain.ShowtimeSeat, error)
}

// Cache keeps seat maps and day schedules for a short time. It is implemented by repository/redis.CatalogCache.
// Any method may fail; the service then reads from the Repository, so a cache outage costs latency, not
// correctness. Cached values may be up to the cache's TTL old, and times in them are in any zone.
type Cache interface {
	SeatMap(ctx context.Context, showtimeID int64) (SeatMap, bool, error)
	SetSeatMap(ctx context.Context, showtimeID int64, sm SeatMap) error
	// Schedule and SetSchedule store every showtime of a day, named yyyy-mm-dd in the cinema's time zone.
	Schedule(ctx context.Context, day string) ([]domain.Showtime, bool, error)
	SetSchedule(ctx context.Context, day string, showtimes []domain.Showtime) error
}

// Service serves catalog queries. All returned times are in the cinema's time zone.
type Service struct {
	repo    Repository
	cache   Cache
	loc     *time.Location
	now     func() time.Time
	flights singleflight.Group
}

// Option customizes a Service.
type Option func(*Service)

// WithClock replaces time.Now, for tests.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithCache serves schedules and seat maps from c when it has them.
func WithCache(c Cache) Option {
	return func(s *Service) { s.cache = c }
}

// New returns a catalog Service. loc is the cinema's time zone: schedule days start at local midnight. Without
// WithCache, every query reads from repo.
func New(repo Repository, loc *time.Location, opts ...Option) *Service {
	s := &Service{repo: repo, cache: noCache{}, loc: loc, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// MoviePage is one page of movies. NextAfterID is 0 when there are no more pages.
type MoviePage struct {
	Movies      []domain.Movie
	NextAfterID int64
}

// ListMovies returns the movies after afterID. A limit outside [1, MaxPageSize] is clamped.
func (s *Service) ListMovies(ctx context.Context, afterID int64, limit int) (MoviePage, error) {
	limit = clampPageSize(limit)

	// Fetch one extra row to learn whether another page exists without a COUNT query.
	movies, err := s.repo.ListMovies(ctx, afterID, limit+1)
	if err != nil {
		return MoviePage{}, err
	}

	page := MoviePage{Movies: movies}
	if len(movies) > limit {
		page.Movies = movies[:limit]
		page.NextAfterID = page.Movies[limit-1].ID
	}
	return page, nil
}

// MovieDetails is a movie with its upcoming showtimes.
type MovieDetails struct {
	Movie    domain.Movie
	Upcoming []domain.Showtime
}

// GetMovie returns a movie and the showtimes that start within the next two weeks.
func (s *Service) GetMovie(ctx context.Context, id int64) (MovieDetails, error) {
	movie, err := s.repo.GetMovie(ctx, id)
	if err != nil {
		return MovieDetails{}, err
	}

	now := s.now()
	upcoming, err := s.repo.ListShowtimes(ctx, domain.ShowtimeFilter{
		From:    now,
		To:      now.Add(upcomingWindow),
		MovieID: id,
		Limit:   upcomingLimit,
	})
	if err != nil {
		return MovieDetails{}, fmt.Errorf("upcoming showtimes of movie %d: %w", id, err)
	}

	return MovieDetails{Movie: movie, Upcoming: s.localizeAll(upcoming)}, nil
}

// ScheduleQuery selects the schedule of one day.
type ScheduleQuery struct {
	// Day is the calendar day to list; only its year, month, and day are used. Zero means today.
	Day     time.Time
	MovieID int64 // 0 means all movies
}

// Schedule is the list of showtimes of one cinema day.
type Schedule struct {
	Day       time.Time // local midnight that starts the day
	Showtimes []domain.Showtime
}

// Schedule returns the showtimes that start on the given local calendar day. The whole day is read and cached
// as one entry, and the movie filter applies to it afterwards, so all filters of a day share the entry.
func (s *Service) Schedule(ctx context.Context, q ScheduleQuery) (Schedule, error) {
	day := q.Day
	if day.IsZero() {
		day = s.now().In(s.loc)
	}
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, s.loc)
	// AddDate, not Add(24h): a day with a daylight-saving change is 23 or 25 hours long.
	end := start.AddDate(0, 0, 1)
	key := start.Format(time.DateOnly)

	all, ok, _ := s.cache.Schedule(ctx, key) // a failed lookup counts as a miss
	if !ok {
		var err error
		all, err = shared(ctx, &s.flights, "schedule:"+key, func(ctx context.Context) ([]domain.Showtime, error) {
			all, err := s.repo.ListShowtimes(ctx, domain.ShowtimeFilter{From: start, To: end})
			if err == nil {
				_ = s.cache.SetSchedule(ctx, key, all) // best effort: the next miss tries again
			}
			return all, err
		})
		if err != nil {
			return Schedule{}, err
		}
	}

	// The list may be shared with concurrent callers, so it is copied, never changed in place.
	showtimes := make([]domain.Showtime, 0, len(all))
	for _, st := range all {
		if q.MovieID == 0 || st.Movie.ID == q.MovieID {
			showtimes = append(showtimes, s.localize(st))
		}
	}
	return Schedule{Day: start, Showtimes: showtimes}, nil
}

// GetShowtime returns one showtime.
func (s *Service) GetShowtime(ctx context.Context, id int64) (domain.Showtime, error) {
	st, err := s.repo.GetShowtime(ctx, id)
	if err != nil {
		return domain.Showtime{}, err
	}
	return s.localize(st), nil
}

// SeatSummary counts the seats of a showtime by status.
type SeatSummary struct {
	Available int
	Held      int
	Sold      int
	Total     int
}

// SeatMap is the seat layout of a showtime with each seat's status and price.
type SeatMap struct {
	Showtime domain.Showtime
	Seats    []domain.ShowtimeSeat
	Summary  SeatSummary
}

// SeatMap returns the seats of a showtime. The summary is computed from the same rows as the seats, so the
// two are always consistent with each other.
//
// A cached seat map is at most the cache's TTL old, and usually fresher: the booking service deletes it after
// every committed change to the showtime's seats. Staleness never affects bookings, which check the seats in
// PostgreSQL under row locks.
func (s *Service) SeatMap(ctx context.Context, showtimeID int64) (SeatMap, error) {
	sm, ok, _ := s.cache.SeatMap(ctx, showtimeID) // a failed lookup counts as a miss
	if !ok {
		var err error
		sm, err = shared(ctx, &s.flights, "seatmap:"+strconv.FormatInt(showtimeID, 10),
			func(ctx context.Context) (SeatMap, error) {
				sm, err := s.loadSeatMap(ctx, showtimeID)
				if err == nil {
					_ = s.cache.SetSeatMap(ctx, showtimeID, sm) // best effort: the next miss tries again
				}
				return sm, err
			})
		if err != nil {
			return SeatMap{}, err
		}
	}
	// Showtime is a copy; the seats may be shared with concurrent callers and are only read.
	sm.Showtime = s.localize(sm.Showtime)
	return sm, nil
}

func (s *Service) loadSeatMap(ctx context.Context, showtimeID int64) (SeatMap, error) {
	st, err := s.repo.GetShowtime(ctx, showtimeID)
	if err != nil {
		return SeatMap{}, err
	}
	seats, err := s.repo.ListShowtimeSeats(ctx, showtimeID)
	if err != nil {
		return SeatMap{}, err
	}

	summary := SeatSummary{Total: len(seats)}
	for _, seat := range seats {
		switch seat.Status {
		case domain.SeatAvailable:
			summary.Available++
		case domain.SeatHeld:
			summary.Held++
		case domain.SeatSold:
			summary.Sold++
		}
	}

	st.SeatsAvailable, st.SeatsTotal = summary.Available, summary.Total
	return SeatMap{Showtime: st, Seats: seats, Summary: summary}, nil
}

// shared runs load once for all concurrent callers that pass the same key, and hands each of them the result.
// load runs on a context that no caller can cancel, bounded by loadTimeout; a caller whose own context ends stops
// waiting and gets its context's error. The result may be shared, so callers must not change it.
func shared[T any](ctx context.Context, flights *singleflight.Group, key string, load func(context.Context) (T, error)) (T, error) {
	ch := flights.DoChan(key, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loadTimeout)
		defer cancel()
		return load(ctx)
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			var zero T
			return zero, res.Err
		}
		return res.Val.(T), nil
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// noCache is the Cache of a Service built without WithCache: it never has anything.
type noCache struct{}

func (noCache) SeatMap(context.Context, int64) (SeatMap, bool, error) { return SeatMap{}, false, nil }
func (noCache) SetSeatMap(context.Context, int64, SeatMap) error      { return nil }
func (noCache) Schedule(context.Context, string) ([]domain.Showtime, bool, error) {
	return nil, false, nil
}
func (noCache) SetSchedule(context.Context, string, []domain.Showtime) error { return nil }

func (s *Service) localize(st domain.Showtime) domain.Showtime {
	st.StartsAt = st.StartsAt.In(s.loc)
	st.EndsAt = st.EndsAt.In(s.loc)
	return st
}

func (s *Service) localizeAll(showtimes []domain.Showtime) []domain.Showtime {
	for i := range showtimes {
		showtimes[i] = s.localize(showtimes[i])
	}
	return showtimes
}

func clampPageSize(limit int) int {
	switch {
	case limit <= 0:
		return DefaultPageSize
	case limit > MaxPageSize:
		return MaxPageSize
	default:
		return limit
	}
}
