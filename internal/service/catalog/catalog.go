// Package catalog implements the read-only browsing use cases: movies, the schedule, and seat maps.
package catalog

import (
	"context"
	"fmt"
	"time"

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

// Repository is the storage the catalog needs. It is implemented by repository/postgres.Catalog.
type Repository interface {
	ListMovies(ctx context.Context, afterID int64, limit int) ([]domain.Movie, error)
	GetMovie(ctx context.Context, id int64) (domain.Movie, error)
	ListShowtimes(ctx context.Context, f domain.ShowtimeFilter) ([]domain.Showtime, error)
	GetShowtime(ctx context.Context, id int64) (domain.Showtime, error)
	ListShowtimeSeats(ctx context.Context, showtimeID int64) ([]domain.ShowtimeSeat, error)
}

// Service serves catalog queries. All returned times are in the cinema's time zone.
type Service struct {
	repo Repository
	loc  *time.Location
	now  func() time.Time
}

// Option customizes a Service.
type Option func(*Service)

// WithClock replaces time.Now, for tests.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// New returns a catalog Service. loc is the cinema's time zone: schedule days start at local midnight.
func New(repo Repository, loc *time.Location, opts ...Option) *Service {
	s := &Service{repo: repo, loc: loc, now: time.Now}
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

// Schedule returns the showtimes that start on the given local calendar day.
func (s *Service) Schedule(ctx context.Context, q ScheduleQuery) (Schedule, error) {
	day := q.Day
	if day.IsZero() {
		day = s.now().In(s.loc)
	}
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, s.loc)
	// AddDate, not Add(24h): a day with a daylight-saving change is 23 or 25 hours long.
	end := start.AddDate(0, 0, 1)

	showtimes, err := s.repo.ListShowtimes(ctx, domain.ShowtimeFilter{From: start, To: end, MovieID: q.MovieID})
	if err != nil {
		return Schedule{}, err
	}
	return Schedule{Day: start, Showtimes: s.localizeAll(showtimes)}, nil
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
func (s *Service) SeatMap(ctx context.Context, showtimeID int64) (SeatMap, error) {
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

	st = s.localize(st)
	st.SeatsAvailable, st.SeatsTotal = summary.Available, summary.Total
	return SeatMap{Showtime: st, Seats: seats, Summary: summary}, nil
}

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
