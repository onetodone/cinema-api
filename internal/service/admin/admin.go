// Package admin implements the catalog management use cases that only administrators may run.
package admin

import (
	"context"
	"strings"
	"time"

	"github.com/onetodone/cinema-api/internal/domain"
)

// Repository is the catalog storage the admin use cases write to. It is implemented by
// repository/postgres.Catalog.
type Repository interface {
	CreateMovie(ctx context.Context, m domain.NewMovie) (domain.Movie, error)
	// CreateHall stores a hall and generates its seats. A name that another hall has fails with HALL_NAME_TAKEN.
	CreateHall(ctx context.Context, name string, rows []domain.HallRow) (domain.HallLayout, error)
	// CreateShowtime stores a showtime and its seat inventory in one transaction. It fails with MOVIE_NOT_FOUND,
	// HALL_NOT_FOUND, or HALL_OVERLAP when another scheduled showtime occupies the hall at an overlapping time.
	CreateShowtime(ctx context.Context, s domain.NewShowtime) (domain.Showtime, error)
}

// ScheduleCache is told when a showtime is added, so that the cached schedule of its day lists it at once instead
// of when the entry expires. It is implemented by repository/redis.CatalogCache. A failure leaves the cached
// schedule to expire on its own.
type ScheduleCache interface {
	// InvalidateSchedule deletes the cached schedule of a day, named yyyy-mm-dd in the cinema's time zone.
	InvalidateSchedule(ctx context.Context, day string) error
}

// Service manages the catalog. Callers must check that the caller is an admin; the HTTP layer does that.
type Service struct {
	repo      Repository
	loc       *time.Location
	now       func() time.Time
	schedules ScheduleCache // nil: no cached schedules to invalidate
}

// Option customizes a Service.
type Option func(*Service)

// WithClock replaces time.Now, for tests.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithScheduleCache invalidates the cached schedule of a day in c when a showtime is added to it.
func WithScheduleCache(c ScheduleCache) Option {
	return func(s *Service) { s.schedules = c }
}

// New returns an admin Service. loc is the cinema's time zone: it names schedule days, and returned showtimes
// carry its offset.
func New(repo Repository, loc *time.Location, opts ...Option) *Service {
	s := &Service{repo: repo, loc: loc, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// CreateMovie adds a movie. Surrounding space is trimmed from every text field. Invalid input fails with a
// *domain.ValidationError listing every bad field.
func (s *Service) CreateMovie(ctx context.Context, m domain.NewMovie) (domain.Movie, error) {
	m.Title = strings.TrimSpace(m.Title)
	m.Description = strings.TrimSpace(m.Description)
	m.AgeRating = strings.TrimSpace(m.AgeRating)
	m.PosterURL = strings.TrimSpace(m.PosterURL)
	if err := m.Validate(); err != nil {
		return domain.Movie{}, err
	}
	return s.repo.CreateMovie(ctx, m)
}

// CreateHall adds a hall with its seats and returns them, in seat map order. Surrounding space is trimmed from the
// name, and a row without a type holds standard seats. Invalid input fails with a *domain.ValidationError listing
// every bad field; a name that another hall has fails with HALL_NAME_TAKEN.
func (s *Service) CreateHall(ctx context.Context, h domain.NewHall) (domain.HallLayout, error) {
	h.Name = strings.TrimSpace(h.Name)
	rows := make([]domain.HallRow, len(h.Rows)) // the caller's slice stays as it was
	for i, r := range h.Rows {
		if r.Type == "" {
			r.Type = domain.SeatStandard
		}
		rows[i] = r
	}
	h.Rows = rows
	if err := h.Validate(); err != nil {
		return domain.HallLayout{}, err
	}
	return s.repo.CreateHall(ctx, h.Name, h.Rows)
}

// CreateShowtime schedules a movie in a hall and makes every seat of the hall available for it. The showtime
// occupies the hall from its start until the movie and the cleaning buffer are over; a hall cannot host two
// scheduled showtimes that overlap, which the database enforces with an exclusion constraint, so two admins who
// schedule the same slot at once cannot both succeed. Surrounding space is trimmed from the language codes, and
// they are lower-cased.
//
// Errors: a *domain.ValidationError; MOVIE_NOT_FOUND; HALL_NOT_FOUND; HALL_OVERLAP.
func (s *Service) CreateShowtime(ctx context.Context, ns domain.NewShowtime) (domain.Showtime, error) {
	ns.Language.Audio = strings.ToLower(strings.TrimSpace(ns.Language.Audio))
	ns.Language.Subtitles = strings.ToLower(strings.TrimSpace(ns.Language.Subtitles))
	if err := ns.Validate(s.now()); err != nil {
		return domain.Showtime{}, err
	}
	st, err := s.repo.CreateShowtime(ctx, ns)
	if err != nil {
		return domain.Showtime{}, err
	}

	st.StartsAt, st.EndsAt = st.StartsAt.In(s.loc), st.EndsAt.In(s.loc)
	if s.schedules != nil {
		// The showtime exists now; a client that hangs up must not leave the old schedule cached.
		day := st.StartsAt.Format(time.DateOnly)
		_ = s.schedules.InvalidateSchedule(context.WithoutCancel(ctx), day) // on failure, the entry expires by itself
	}
	return st, nil
}
