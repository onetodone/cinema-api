//go:build integration

package integration

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/onetodone/cinema-api/internal/domain"
)

// base is a fixed, far-future reference time, so tests never depend on the current date.
var base = time.Date(2030, 1, 10, 10, 0, 0, 0, time.UTC)

func TestMoviesKeysetPagination(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()

	third := must(f.catalog.CreateMovie(ctx, domain.NewMovie{Title: "Third", DurationMin: 100}))(t)

	page1 := must(f.catalog.ListMovies(ctx, 0, 2))(t)
	if len(page1) != 2 || page1[0].ID != f.dune.ID || page1[1].ID != f.short.ID {
		t.Fatalf("page 1 = %+v", page1)
	}
	page2 := must(f.catalog.ListMovies(ctx, page1[1].ID, 2))(t)
	if len(page2) != 1 || page2[0].ID != third.ID {
		t.Fatalf("page 2 = %+v", page2)
	}

	got := must(f.catalog.GetMovie(ctx, f.dune.ID))(t)
	if got.Title != "Dune" || got.AgeRating != "PG-13" || got.PosterURL != "" || got.CreatedAt.IsZero() {
		t.Errorf("movie = %+v", got)
	}

	_, err := f.catalog.GetMovie(ctx, 999_999)
	if !errors.Is(err, domain.ErrNotFound) || domainCode(err) != domain.CodeMovieNotFound {
		t.Errorf("err = %v, want MOVIE_NOT_FOUND", err)
	}
}

func TestCreateHallRejectsDuplicateName(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, err := f.catalog.CreateHall(t.Context(), "Hall 1", []domain.HallRow{{Label: "A", Seats: 1, Type: domain.SeatStandard}})
	if !errors.Is(err, domain.ErrConflict) || domainCode(err) != domain.CodeHallNameTaken {
		t.Fatalf("err = %v, want HALL_NAME_TAKEN", err)
	}
}

func TestCreateShowtimeMaterializesInventory(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	st := f.showtime(t, f.dune, f.hall, base)

	wantEnd := base.Add(155*time.Minute + domain.CleaningBuffer)
	if !st.EndsAt.Equal(wantEnd) {
		t.Errorf("ends_at = %s, want start + duration + cleaning buffer = %s", st.EndsAt, wantEnd)
	}
	if st.SeatsTotal != 5 || st.SeatsAvailable != 5 {
		t.Errorf("seats = %d/%d, want 5/5", st.SeatsAvailable, st.SeatsTotal)
	}
	if st.Movie.Title != "Dune" || st.Hall.Name != "Hall 1" || st.Status != domain.ShowtimeScheduled {
		t.Errorf("showtime = %+v", st)
	}

	seats := must(f.catalog.ListShowtimeSeats(t.Context(), st.ID))(t)
	for _, s := range seats {
		if want := domain.SeatPrice(1000, s.Type); s.PriceCents != want {
			t.Errorf("seat %s%d (%s) costs %d, want %d", s.Row, s.Number, s.Type, s.PriceCents, want)
		}
		if s.Status != domain.SeatAvailable {
			t.Errorf("seat %s%d is %s, want available", s.Row, s.Number, s.Status)
		}
	}
}

func TestCreateShowtimeErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	first := f.showtime(t, f.dune, f.hall, base) // occupies [10:00, 12:50)

	tests := []struct {
		name string
		ns   domain.NewShowtime
		code string
	}{
		{
			name: "unknown movie",
			ns:   domain.NewShowtime{MovieID: 999_999, HallID: f.hall.ID, StartsAt: base.Add(24 * time.Hour), Language: english},
			code: domain.CodeMovieNotFound,
		},
		{
			name: "unknown hall",
			ns:   domain.NewShowtime{MovieID: f.short.ID, HallID: 999_999, StartsAt: base.Add(24 * time.Hour), Language: english},
			code: domain.CodeHallNotFound,
		},
		{
			name: "overlaps the end of another showtime",
			ns:   domain.NewShowtime{MovieID: f.short.ID, HallID: f.hall.ID, StartsAt: first.EndsAt.Add(-time.Minute), Language: english},
			code: domain.CodeHallOverlap,
		},
		{
			name: "starts before and ends inside another showtime",
			ns:   domain.NewShowtime{MovieID: f.short.ID, HallID: f.hall.ID, StartsAt: base.Add(-time.Hour), Language: english},
			code: domain.CodeHallOverlap,
		},
	}
	for _, tt := range tests {
		_, err := f.catalog.CreateShowtime(ctx, tt.ns)
		if domainCode(err) != tt.code {
			t.Errorf("%s: err = %v, want %s", tt.name, err, tt.code)
		}
	}

	// Half-open ranges: a showtime may start exactly when the previous one (with cleaning) ends.
	f.showtime(t, f.short, f.hall, first.EndsAt)
	// The same time slot in another hall is fine.
	f.showtime(t, f.short, f.hall2, base)

	// A canceled showtime no longer blocks its slot.
	exec(t, f.pool, `UPDATE showtimes SET status = 'canceled' WHERE id = $1`, first.ID)
	f.showtime(t, f.short, f.hall, base)

	// The database refuses what is not shaped like an ISO 639-3 code, even without the service's validation.
	for _, lv := range []domain.LanguageVersion{{}, {Audio: "EN"}, {Audio: "eng", Subtitles: "th"}} {
		_, err := f.catalog.CreateShowtime(ctx, domain.NewShowtime{
			MovieID: f.short.ID, HallID: f.hall2.ID, StartsAt: base.Add(48 * time.Hour), Language: lv,
		})
		if pgCode(err) != "23514" { // check_violation
			t.Errorf("language %+v: err = %v, want a check violation", lv, err)
		}
	}
}

// TestGenresAndLanguagesRoundTrip stores a movie with genres and a showtime with subtitles and reads them back
// from every query that shows them.
func TestGenresAndLanguagesRoundTrip(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()

	genres := []domain.Genre{domain.GenreScienceFiction, domain.GenreAdventure, domain.GenreDrama}
	movie := must(f.catalog.CreateMovie(ctx, domain.NewMovie{Title: "Dune: Part Two", DurationMin: 166, Genres: genres}))(t)
	if !slices.Equal(movie.Genres, genres) {
		t.Errorf("created genres = %v, want %v in that order", movie.Genres, genres)
	}
	if got := must(f.catalog.GetMovie(ctx, movie.ID))(t); !slices.Equal(got.Genres, genres) {
		t.Errorf("read genres = %v, want %v", got.Genres, genres)
	}
	if got := must(f.catalog.GetMovie(ctx, f.short.ID))(t); got.Genres == nil || len(got.Genres) != 0 {
		t.Errorf("genres of a movie without any = %#v, want empty", got.Genres)
	}

	thaiSubs := domain.LanguageVersion{Audio: "eng", Subtitles: "tha"}
	st, err := f.catalog.CreateShowtime(ctx, domain.NewShowtime{
		MovieID: movie.ID, HallID: f.hall.ID, StartsAt: base, Language: thaiSubs, BasePriceCents: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Language != thaiSubs || !slices.Equal(st.Movie.Genres, genres) {
		t.Errorf("created showtime = %+v, want %+v and the movie's genres", st, thaiSubs)
	}
	dubbed := f.showtime(t, f.short, f.hall2, base)
	if dubbed.Language != english {
		t.Errorf("showtime without subtitles = %+v, want %+v", dubbed.Language, english)
	}

	list := must(f.catalog.ListShowtimes(ctx, domain.ShowtimeFilter{From: base, To: base.Add(time.Hour)}))(t)
	if len(list) != 2 || list[0].Language != thaiSubs || list[1].Language != english || !slices.Equal(list[0].Movie.Genres, genres) {
		t.Errorf("schedule = %+v", list)
	}
	if n := countRows(t, f.pool, `SELECT count(*) FROM showtimes WHERE subtitle_language IS NULL`); n != 1 {
		t.Errorf("%d showtimes store NULL subtitles, want the one without them", n)
	}
}

// TestGenreEnumMatchesTheDomain keeps the movie_genre enum and domain.Genres() equal, in the same order.
func TestGenreEnumMatchesTheDomain(t *testing.T) {
	t.Parallel()
	pool := newDB(t)

	var inDB []string
	if err := pool.QueryRow(t.Context(), `SELECT enum_range(NULL::movie_genre)::text[]`).Scan(&inDB); err != nil {
		t.Fatal(err)
	}
	var inDomain []string
	for _, g := range domain.Genres() {
		inDomain = append(inDomain, string(g))
	}
	if !slices.Equal(inDB, inDomain) {
		t.Errorf("movie_genre = %v\ndomain     = %v", inDB, inDomain)
	}
}

func TestListShowtimesFiltersAndCounts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()

	morning := f.showtime(t, f.dune, f.hall, base)
	other := f.showtime(t, f.short, f.hall2, base.Add(time.Hour))
	nextDay := f.showtime(t, f.short, f.hall, base.Add(24*time.Hour))

	// One seat held and one sold in the morning showtime.
	seats := must(f.catalog.ListShowtimeSeats(ctx, morning.ID))(t)
	f.setSeatStatus(t, morning.ID, seats[0].SeatID, domain.SeatHeld)
	f.setSeatStatus(t, morning.ID, seats[1].SeatID, domain.SeatSold)

	day := domain.ShowtimeFilter{From: base.Truncate(24 * time.Hour), To: base.Truncate(24 * time.Hour).Add(24 * time.Hour)}
	list := must(f.catalog.ListShowtimes(ctx, day))(t)
	if len(list) != 2 || list[0].ID != morning.ID || list[1].ID != other.ID {
		t.Fatalf("day list = %+v, want [morning, other] ordered by start", list)
	}
	if list[0].SeatsAvailable != 3 || list[0].SeatsTotal != 5 {
		t.Errorf("morning seats = %d/%d, want 3/5 after one hold and one sale", list[0].SeatsAvailable, list[0].SeatsTotal)
	}

	byMovie := day
	byMovie.MovieID = f.short.ID
	if list := must(f.catalog.ListShowtimes(ctx, byMovie))(t); len(list) != 1 || list[0].ID != other.ID {
		t.Errorf("movie filter = %+v, want only the short movie", list)
	}

	// From is inclusive and To is exclusive.
	exact := domain.ShowtimeFilter{From: nextDay.StartsAt, To: nextDay.StartsAt.Add(time.Minute)}
	if list := must(f.catalog.ListShowtimes(ctx, exact))(t); len(list) != 1 || list[0].ID != nextDay.ID {
		t.Errorf("inclusive From: got %+v", list)
	}
	before := domain.ShowtimeFilter{From: base.Add(-time.Hour), To: base}
	if list := must(f.catalog.ListShowtimes(ctx, before))(t); len(list) != 0 {
		t.Errorf("exclusive To: got %+v", list)
	}

	limited := domain.ShowtimeFilter{From: base.Add(-time.Hour), To: base.Add(48 * time.Hour), Limit: 2}
	if list := must(f.catalog.ListShowtimes(ctx, limited))(t); len(list) != 2 {
		t.Errorf("limit 2: got %d showtimes", len(list))
	}

	// Canceled showtimes disappear from the schedule but can still be fetched directly.
	exec(t, f.pool, `UPDATE showtimes SET status = 'canceled' WHERE id = $1`, other.ID)
	if list := must(f.catalog.ListShowtimes(ctx, day))(t); len(list) != 1 {
		t.Errorf("after cancel: %d showtimes, want 1", len(list))
	}
	if st := must(f.catalog.GetShowtime(ctx, other.ID))(t); st.Status != domain.ShowtimeCanceled {
		t.Errorf("status = %s, want canceled", st.Status)
	}

	_, err := f.catalog.GetShowtime(ctx, 999_999)
	if domainCode(err) != domain.CodeShowtimeNotFound {
		t.Errorf("err = %v, want SHOWTIME_NOT_FOUND", err)
	}
}

func TestSeatMapOrdering(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	st := f.showtime(t, f.dune, f.hall, base)
	seats := must(f.catalog.ListShowtimeSeats(t.Context(), st.ID))(t)

	var got []string
	for _, s := range seats {
		got = append(got, s.Row+string(rune('0'+s.Number)))
	}
	want := []string{"A1", "A2", "B1", "B2", "AA1"}
	if len(got) != len(want) {
		t.Fatalf("seats = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("seats = %v, want %v (rows by label length, then label, then number)", got, want)
		}
	}
}

// TestSeatStatusMustMatchBooking checks the schema-level guard: a seat has a booking exactly when it is not
// available. Even a buggy code path cannot mark a seat held without saying who holds it.
func TestSeatStatusMustMatchBooking(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	st := f.showtime(t, f.dune, f.hall, base)
	seats := must(f.catalog.ListShowtimeSeats(ctx, st.ID))(t)

	_, err := f.pool.Exec(ctx, `UPDATE showtime_seats SET status = 'held' WHERE showtime_id = $1 AND seat_id = $2`,
		st.ID, seats[0].SeatID)
	assertCheckViolation(t, err)

	f.setSeatStatus(t, st.ID, seats[1].SeatID, domain.SeatHeld)
	_, err = f.pool.Exec(ctx, `UPDATE showtime_seats SET status = 'available' WHERE showtime_id = $1 AND seat_id = $2`,
		st.ID, seats[1].SeatID)
	assertCheckViolation(t, err)
}

func assertCheckViolation(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "showtime_seats_booking_matches_status" {
		t.Fatalf("err = %v, want a violation of showtime_seats_booking_matches_status", err)
	}
}

// pgCode returns the SQLSTATE of a PostgreSQL error in err's chain, or "".
func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
