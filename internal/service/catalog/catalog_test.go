package catalog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/onetodone/cinema-api/internal/domain"
)

// fakeRepo is an in-memory Repository that records the filters it receives.
type fakeRepo struct {
	movies    []domain.Movie
	showtimes map[int64]domain.Showtime
	seats     map[int64][]domain.ShowtimeSeat
	listed    []domain.ShowtimeFilter
	err       error
}

func (f *fakeRepo) ListMovies(_ context.Context, afterID int64, limit int) ([]domain.Movie, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []domain.Movie
	for _, m := range f.movies {
		if m.ID > afterID && len(out) < limit {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeRepo) GetMovie(_ context.Context, id int64) (domain.Movie, error) {
	for _, m := range f.movies {
		if m.ID == id {
			return m, nil
		}
	}
	return domain.Movie{}, domain.NotFound(domain.CodeMovieNotFound, "movie %d not found", id)
}

func (f *fakeRepo) ListShowtimes(_ context.Context, filter domain.ShowtimeFilter) ([]domain.Showtime, error) {
	f.listed = append(f.listed, filter)
	if f.err != nil {
		return nil, f.err
	}
	var out []domain.Showtime
	for _, st := range f.showtimes {
		if !st.StartsAt.Before(filter.From) && st.StartsAt.Before(filter.To) {
			out = append(out, st)
		}
	}
	return out, nil
}

func (f *fakeRepo) GetShowtime(_ context.Context, id int64) (domain.Showtime, error) {
	st, ok := f.showtimes[id]
	if !ok {
		return domain.Showtime{}, domain.NotFound(domain.CodeShowtimeNotFound, "showtime %d not found", id)
	}
	return st, nil
}

func (f *fakeRepo) ListShowtimeSeats(_ context.Context, showtimeID int64) ([]domain.ShowtimeSeat, error) {
	return f.seats[showtimeID], nil
}

func movies(n int) []domain.Movie {
	out := make([]domain.Movie, n)
	for i := range out {
		out[i] = domain.Movie{ID: int64(i + 1), Title: "Movie"}
	}
	return out
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load location %s: %v", name, err)
	}
	return loc
}

func TestListMoviesPaginates(t *testing.T) {
	t.Parallel()

	svc := New(&fakeRepo{movies: movies(5)}, time.UTC)

	first, err := svc.ListMovies(t.Context(), 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Movies) != 2 || first.NextAfterID != 2 {
		t.Fatalf("first page = %d movies, next %d; want 2 movies, next 2", len(first.Movies), first.NextAfterID)
	}

	last, err := svc.ListMovies(t.Context(), 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(last.Movies) != 1 || last.Movies[0].ID != 5 || last.NextAfterID != 0 {
		t.Fatalf("last page = %+v, want only movie 5 and no next page", last)
	}
}

func TestListMoviesExactPageHasNoNextCursor(t *testing.T) {
	t.Parallel()

	page, err := New(&fakeRepo{movies: movies(2)}, time.UTC).ListMovies(t.Context(), 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Movies) != 2 || page.NextAfterID != 0 {
		t.Fatalf("page = %d movies, next %d; want 2 movies and no next page", len(page.Movies), page.NextAfterID)
	}
}

func TestClampPageSize(t *testing.T) {
	t.Parallel()

	for in, want := range map[int]int{-1: DefaultPageSize, 0: DefaultPageSize, 1: 1, 100: 100, 101: MaxPageSize} {
		if got := clampPageSize(in); got != want {
			t.Errorf("clampPageSize(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestScheduleUsesLocalCalendarDay(t *testing.T) {
	t.Parallel()

	loc := mustLoad(t, "Asia/Dubai") // UTC+4, no daylight saving
	repo := &fakeRepo{showtimes: map[int64]domain.Showtime{
		1: {ID: 1, StartsAt: time.Date(2026, 10, 1, 19, 59, 0, 0, time.UTC)}, // 23:59 local on Oct 1
		2: {ID: 2, StartsAt: time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)},  // 00:00 local on Oct 2
	}}
	svc := New(repo, loc)

	sched, err := svc.Schedule(t.Context(), ScheduleQuery{Day: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}

	f := repo.listed[0]
	wantFrom := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	if !f.From.Equal(wantFrom) || !f.To.Equal(wantFrom.AddDate(0, 0, 1)) {
		t.Errorf("filter = [%s, %s), want the local day starting %s", f.From, f.To, wantFrom)
	}
	if len(sched.Showtimes) != 1 || sched.Showtimes[0].ID != 1 {
		t.Fatalf("showtimes = %+v, want only showtime 1", sched.Showtimes)
	}
	if got := sched.Showtimes[0].StartsAt.Location(); got != loc {
		t.Errorf("start time location = %s, want %s", got, loc)
	}
}

func TestScheduleHandlesDaylightSavingDays(t *testing.T) {
	t.Parallel()

	loc := mustLoad(t, "Europe/Berlin")
	repo := &fakeRepo{}
	// Clocks go back on 2026-10-25, so that local day is 25 hours long.
	_, err := New(repo, loc).Schedule(t.Context(), ScheduleQuery{Day: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}

	f := repo.listed[0]
	if got := f.To.Sub(f.From); got != 25*time.Hour {
		t.Errorf("day length = %s, want 25h", got)
	}
}

func TestScheduleDefaultsToToday(t *testing.T) {
	t.Parallel()

	loc := mustLoad(t, "Asia/Dubai")
	now := time.Date(2026, 10, 1, 21, 0, 0, 0, time.UTC) // already Oct 2 in Dubai
	repo := &fakeRepo{}
	sched, err := New(repo, loc, WithClock(func() time.Time { return now })).Schedule(t.Context(), ScheduleQuery{})
	if err != nil {
		t.Fatal(err)
	}

	if want := time.Date(2026, 10, 2, 0, 0, 0, 0, loc); !sched.Day.Equal(want) {
		t.Errorf("day = %s, want %s", sched.Day, want)
	}
}

func TestScheduleFiltersByMovie(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{}
	if _, err := New(repo, time.UTC).Schedule(t.Context(), ScheduleQuery{MovieID: 7}); err != nil {
		t.Fatal(err)
	}
	if repo.listed[0].MovieID != 7 {
		t.Errorf("movie filter = %d, want 7", repo.listed[0].MovieID)
	}
}

func TestGetMovieIncludesUpcomingShowtimes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepo{
		movies: movies(1),
		showtimes: map[int64]domain.Showtime{
			1: {ID: 1, StartsAt: now.Add(-time.Hour)},          // already started
			2: {ID: 2, StartsAt: now.Add(time.Hour)},           // upcoming
			3: {ID: 3, StartsAt: now.Add(15 * 24 * time.Hour)}, // beyond the window
		},
	}
	details, err := New(repo, time.UTC, WithClock(func() time.Time { return now })).GetMovie(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}

	f := repo.listed[0]
	if f.MovieID != 1 || !f.From.Equal(now) || f.To.Sub(f.From) != upcomingWindow || f.Limit != upcomingLimit {
		t.Errorf("filter = %+v", f)
	}
	if len(details.Upcoming) != 1 || details.Upcoming[0].ID != 2 {
		t.Errorf("upcoming = %+v, want only showtime 2", details.Upcoming)
	}
}

func TestGetMovieNotFound(t *testing.T) {
	t.Parallel()

	_, err := New(&fakeRepo{}, time.UTC).GetMovie(t.Context(), 99)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSeatMapSummarizesStatuses(t *testing.T) {
	t.Parallel()

	loc := mustLoad(t, "Asia/Dubai")
	repo := &fakeRepo{
		showtimes: map[int64]domain.Showtime{
			5: {ID: 5, StartsAt: time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC), SeatsAvailable: 99, SeatsTotal: 99},
		},
		seats: map[int64][]domain.ShowtimeSeat{5: {
			{SeatID: 1, Status: domain.SeatAvailable},
			{SeatID: 2, Status: domain.SeatAvailable},
			{SeatID: 3, Status: domain.SeatHeld},
			{SeatID: 4, Status: domain.SeatSold},
		}},
	}

	sm, err := New(repo, loc).SeatMap(t.Context(), 5)
	if err != nil {
		t.Fatal(err)
	}

	want := SeatSummary{Available: 2, Held: 1, Sold: 1, Total: 4}
	if sm.Summary != want {
		t.Errorf("summary = %+v, want %+v", sm.Summary, want)
	}
	if sm.Showtime.SeatsAvailable != 2 || sm.Showtime.SeatsTotal != 4 {
		t.Errorf("showtime counts = %d/%d, want them to match the seat rows (2/4)",
			sm.Showtime.SeatsAvailable, sm.Showtime.SeatsTotal)
	}
	if sm.Showtime.StartsAt.Location() != loc {
		t.Errorf("start time is not localized")
	}
}

func TestSeatMapUnknownShowtime(t *testing.T) {
	t.Parallel()

	_, err := New(&fakeRepo{}, time.UTC).SeatMap(t.Context(), 1)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRepositoryErrorsPropagate(t *testing.T) {
	t.Parallel()

	boom := errors.New("db down")
	svc := New(&fakeRepo{err: boom}, time.UTC)

	if _, err := svc.ListMovies(t.Context(), 0, 10); !errors.Is(err, boom) {
		t.Errorf("ListMovies err = %v, want %v", err, boom)
	}
	if _, err := svc.Schedule(t.Context(), ScheduleQuery{}); !errors.Is(err, boom) {
		t.Errorf("Schedule err = %v, want %v", err, boom)
	}
}
