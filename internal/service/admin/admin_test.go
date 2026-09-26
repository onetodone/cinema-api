package admin

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/onetodone/cinema-api/internal/domain"
)

// recordingRepo remembers what it was asked to create.
type recordingRepo struct {
	movie    *domain.NewMovie
	hall     *domain.NewHall
	showtime *domain.NewShowtime
	err      error
	calls    int
}

func (r *recordingRepo) CreateMovie(_ context.Context, m domain.NewMovie) (domain.Movie, error) {
	r.calls++
	r.movie = &m
	if r.err != nil {
		return domain.Movie{}, r.err
	}
	return domain.Movie{ID: 7, Title: m.Title, DurationMin: m.DurationMin}, nil
}

func (r *recordingRepo) CreateHall(_ context.Context, name string, rows []domain.HallRow) (domain.HallLayout, error) {
	r.calls++
	r.hall = &domain.NewHall{Name: name, Rows: rows}
	if r.err != nil {
		return domain.HallLayout{}, r.err
	}
	return domain.HallLayout{Hall: domain.Hall{ID: 3, Name: name}}, nil
}

func (r *recordingRepo) CreateShowtime(_ context.Context, s domain.NewShowtime) (domain.Showtime, error) {
	r.calls++
	r.showtime = &s
	if r.err != nil {
		return domain.Showtime{}, r.err
	}
	return domain.Showtime{ID: 11, StartsAt: s.StartsAt, EndsAt: s.StartsAt.Add(2 * time.Hour)}, nil
}

// recordingCache remembers the days whose schedules were invalidated.
type recordingCache struct {
	days []string
	err  error
}

func (c *recordingCache) InvalidateSchedule(ctx context.Context, day string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.days = append(c.days, day)
	return c.err
}

// now is the fixed clock of the tests: 2030-03-01 12:00 UTC.
var now = time.Date(2030, 3, 1, 12, 0, 0, 0, time.UTC)

func newService(repo Repository, opts ...Option) *Service {
	return New(repo, time.UTC, append([]Option{WithClock(func() time.Time { return now })}, opts...)...)
}

func TestCreateMovieTrimsAndStores(t *testing.T) {
	t.Parallel()

	repo := &recordingRepo{}
	m, err := newService(repo).CreateMovie(t.Context(), domain.NewMovie{
		Title:       "  Dune  ",
		Description: "\tSand.\n",
		DurationMin: 155,
		AgeRating:   " PG-13 ",
		PosterURL:   " https://img.example/dune.jpg ",
	})
	if err != nil {
		t.Fatalf("CreateMovie: %v", err)
	}
	if m.ID != 7 {
		t.Errorf("id = %d, want the repository's 7", m.ID)
	}
	want := domain.NewMovie{
		Title: "Dune", Description: "Sand.", DurationMin: 155, AgeRating: "PG-13",
		PosterURL: "https://img.example/dune.jpg",
	}
	if *repo.movie != want {
		t.Errorf("stored %+v, want %+v", *repo.movie, want)
	}
}

func TestCreateMovieRejectsInvalidInputBeforeStoring(t *testing.T) {
	t.Parallel()

	repo := &recordingRepo{}
	_, err := newService(repo).CreateMovie(t.Context(), domain.NewMovie{Title: "   ", DurationMin: 0})

	var ve *domain.ValidationError
	if !errors.As(err, &ve) || len(ve.Fields) != 2 {
		t.Fatalf("error = %v, want a validation error for title and duration_min", err)
	}
	if repo.calls != 0 {
		t.Error("an invalid movie reached the repository")
	}
}

func TestCreateMoviePropagatesRepositoryErrors(t *testing.T) {
	t.Parallel()

	repo := &recordingRepo{err: errors.New("connection refused")}
	if _, err := newService(repo).CreateMovie(t.Context(), domain.NewMovie{Title: "Dune", DurationMin: 155}); err == nil {
		t.Error("the repository error was swallowed")
	}
}

func TestCreateHallTrimsDefaultsAndStores(t *testing.T) {
	t.Parallel()

	repo := &recordingRepo{}
	rows := []domain.HallRow{
		{Label: "A", Seats: 10},
		{Label: "B", Seats: 12, Type: domain.SeatVIP},
	}
	hall, err := newService(repo).CreateHall(t.Context(), domain.NewHall{Name: "  Hall 4 ", Rows: rows})
	if err != nil {
		t.Fatalf("CreateHall: %v", err)
	}
	if hall.ID != 3 || hall.Name != "Hall 4" {
		t.Errorf("hall = %+v", hall)
	}
	want := []domain.HallRow{
		{Label: "A", Seats: 10, Type: domain.SeatStandard}, // a row without a type holds standard seats
		{Label: "B", Seats: 12, Type: domain.SeatVIP},
	}
	if repo.hall.Name != "Hall 4" || !slices.Equal(repo.hall.Rows, want) {
		t.Errorf("stored %+v, want Hall 4 with %+v", *repo.hall, want)
	}
	if rows[0].Type != "" {
		t.Error("the caller's rows were changed")
	}
}

func TestCreateHallRejectsInvalidInputBeforeStoring(t *testing.T) {
	t.Parallel()

	repo := &recordingRepo{}
	_, err := newService(repo).CreateHall(t.Context(), domain.NewHall{Name: " ", Rows: []domain.HallRow{
		{Label: "a", Seats: 0, Type: "balcony"},
	}})

	var ve *domain.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a validation error", err)
	}
	var fields []string
	for _, f := range ve.Fields {
		fields = append(fields, f.Field)
	}
	if want := []string{"name", "rows[0].label", "rows[0].seats", "rows[0].type"}; !slices.Equal(fields, want) {
		t.Errorf("invalid fields = %v, want %v", fields, want)
	}
	if repo.calls != 0 {
		t.Error("an invalid hall reached the repository")
	}
}

func TestCreateShowtimeStoresAndInvalidatesTheScheduleDay(t *testing.T) {
	t.Parallel()

	repo, cache := &recordingRepo{}, &recordingCache{}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	svc := New(repo, tokyo, WithClock(func() time.Time { return now }), WithScheduleCache(cache))

	// 2030-03-01 20:00 UTC is already March 2 in Tokyo: the cinema's day decides which schedule changes.
	start := time.Date(2030, 3, 1, 20, 0, 0, 0, time.UTC)
	st, err := svc.CreateShowtime(t.Context(), domain.NewShowtime{MovieID: 1, HallID: 2, StartsAt: start, BasePriceCents: 900})
	if err != nil {
		t.Fatalf("CreateShowtime: %v", err)
	}
	if st.ID != 11 || st.StartsAt.Location() != tokyo || st.EndsAt.Location() != tokyo || !st.StartsAt.Equal(start) {
		t.Errorf("showtime = %+v, want its times in the cinema's zone", st)
	}
	if want := (domain.NewShowtime{MovieID: 1, HallID: 2, StartsAt: start, BasePriceCents: 900}); *repo.showtime != want {
		t.Errorf("stored %+v, want %+v", *repo.showtime, want)
	}
	if !slices.Equal(cache.days, []string{"2030-03-02"}) {
		t.Errorf("invalidated days = %v, want [2030-03-02]", cache.days)
	}
}

func TestCreateShowtimeInvalidatesEvenWhenTheClientHasGone(t *testing.T) {
	t.Parallel()

	cache := &recordingCache{}
	svc := newService(&recordingRepo{}, WithScheduleCache(cache))
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // the repository call is a fake, so only the invalidation sees the canceled context

	_, err := svc.CreateShowtime(ctx, domain.NewShowtime{MovieID: 1, HallID: 2, StartsAt: now.Add(time.Hour)})
	if err != nil || len(cache.days) != 1 {
		t.Errorf("CreateShowtime = %v, invalidated %v; want the invalidation to ignore the cancellation", err, cache.days)
	}
}

func TestCreateShowtimeIgnoresAFailingCache(t *testing.T) {
	t.Parallel()

	svc := newService(&recordingRepo{}, WithScheduleCache(&recordingCache{err: errors.New("redis is down")}))
	if _, err := svc.CreateShowtime(t.Context(), domain.NewShowtime{MovieID: 1, HallID: 2, StartsAt: now.Add(time.Hour)}); err != nil {
		t.Errorf("CreateShowtime = %v, want success: the cached schedule expires by itself", err)
	}
}

func TestCreateShowtimeRejectsInvalidInputBeforeStoring(t *testing.T) {
	t.Parallel()

	repo, cache := &recordingRepo{}, &recordingCache{}
	_, err := newService(repo, WithScheduleCache(cache)).CreateShowtime(t.Context(),
		domain.NewShowtime{StartsAt: now, BasePriceCents: -1})

	var ve *domain.ValidationError
	if !errors.As(err, &ve) || len(ve.Fields) != 4 {
		t.Fatalf("error = %v, want movie_id, hall_id, starts_at, and base_price_cents", err)
	}
	if repo.calls != 0 || len(cache.days) != 0 {
		t.Error("an invalid showtime reached the repository or the cache")
	}
}

func TestCreateShowtimeLeavesTheCacheAloneOnFailure(t *testing.T) {
	t.Parallel()

	repo := &recordingRepo{err: domain.Conflict(domain.CodeHallOverlap, "overlap")}
	cache := &recordingCache{}
	_, err := newService(repo, WithScheduleCache(cache)).CreateShowtime(t.Context(),
		domain.NewShowtime{MovieID: 1, HallID: 2, StartsAt: now.Add(time.Hour)})
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("error = %v, want the repository's conflict", err)
	}
	if len(cache.days) != 0 {
		t.Errorf("invalidated %v after a failed create", cache.days)
	}
}
