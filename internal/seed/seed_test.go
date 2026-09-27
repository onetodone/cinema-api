package seed

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/onetodone/cinema-api/internal/domain"
)

func testMovies() []domain.Movie {
	out := make([]domain.Movie, len(movies))
	for i, m := range movies {
		out[i] = domain.Movie{ID: int64(i + 1), Title: m.Title, DurationMin: m.DurationMin}
	}
	return out
}

func TestPlanDayProducesNonOverlappingShowsWithinOpeningHours(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("Asia/Dubai")
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 2, 0, 0, 0, 0, loc) // a Friday

	for offset := range len(movies) {
		slots := PlanDay(day, offset, testMovies())
		if len(slots) < 4 {
			t.Fatalf("offset %d: %d slots, want a full day of shows", offset, len(slots))
		}
		if h, m := slots[0].StartsAt.Hour(), slots[0].StartsAt.Minute(); h != firstShowHour || m != 0 {
			t.Errorf("offset %d: first show at %02d:%02d, want %02d:00", offset, h, m, firstShowHour)
		}

		for i, s := range slots {
			if s.StartsAt.Location() != loc {
				t.Errorf("slot %d is not in the cinema time zone", i)
			}
			if s.StartsAt.Minute()%15 != 0 {
				t.Errorf("slot %d starts at %s, want a quarter hour", i, s.StartsAt.Format("15:04"))
			}
			last := time.Date(2026, 10, 2, lastStartHour, lastStartMinute, 0, 0, loc)
			if s.StartsAt.After(last) {
				t.Errorf("slot %d starts at %s, after the last start time", i, s.StartsAt.Format("15:04"))
			}
			wantEnd := s.StartsAt.Add(time.Duration(s.Movie.DurationMin)*time.Minute + domain.CleaningBuffer)
			if !s.EndsAt.Equal(wantEnd) {
				t.Errorf("slot %d ends at %s, want %s", i, s.EndsAt, wantEnd)
			}
			if i > 0 && s.StartsAt.Before(slots[i-1].EndsAt) {
				t.Errorf("slot %d starts before slot %d ends", i, i-1)
			}
		}
	}
}

func TestPlanDayRotatesMovies(t *testing.T) {
	t.Parallel()

	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	a := PlanDay(day, 0, testMovies())
	b := PlanDay(day, 1, testMovies())
	if a[0].Movie.ID == b[0].Movie.ID {
		t.Error("different offsets should open the day with different movies")
	}
}

func TestPlanDayWithoutMovies(t *testing.T) {
	t.Parallel()

	if slots := PlanDay(time.Now(), 0, nil); slots != nil {
		t.Errorf("slots = %v, want none", slots)
	}
}

func TestPrice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		at   time.Time
		want int64
	}{
		{at: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC), want: matineePrice},                    // Wednesday matinee
		{at: time.Date(2026, 9, 30, 17, 0, 0, 0, time.UTC), want: eveningPrice},                    // Wednesday evening
		{at: time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC), want: matineePrice + weekendSurcharge}, // Saturday
		{at: time.Date(2026, 10, 4, 20, 0, 0, 0, time.UTC), want: eveningPrice + weekendSurcharge}, // Sunday
	}
	for _, tt := range tests {
		if got := price(tt.at); got != tt.want {
			t.Errorf("price(%s) = %d, want %d", tt.at.Format("Mon 15:04"), got, tt.want)
		}
	}
}

func TestRoundUp(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for in, want := range map[time.Duration]time.Duration{
		0:                0,
		time.Minute:      15 * time.Minute,
		15 * time.Minute: 15 * time.Minute,
		16 * time.Minute: 30 * time.Minute,
	} {
		if got := roundUp(base.Add(in), startGranule); !got.Equal(base.Add(want)) {
			t.Errorf("roundUp(12:00+%s) = %s, want 12:00+%s", in, got.Format("15:04"), want)
		}
	}
}

// memStore records what the seeder creates.
type memStore struct {
	movies    []domain.NewMovie
	halls     []string
	showtimes []domain.NewShowtime
}

func (m *memStore) CreateMovie(_ context.Context, nm domain.NewMovie) (domain.Movie, error) {
	m.movies = append(m.movies, nm)
	return domain.Movie{ID: int64(len(m.movies)), Title: nm.Title, DurationMin: nm.DurationMin}, nil
}

func (m *memStore) CreateHall(_ context.Context, name string, _ []domain.HallRow) (domain.HallLayout, error) {
	m.halls = append(m.halls, name)
	return domain.HallLayout{Hall: domain.Hall{ID: int64(len(m.halls)), Name: name}}, nil
}

func (m *memStore) CreateShowtime(_ context.Context, ns domain.NewShowtime) (domain.Showtime, error) {
	m.showtimes = append(m.showtimes, ns)
	return domain.Showtime{}, nil
}

func TestRunCreatesTheWholeCatalog(t *testing.T) {
	t.Parallel()

	store := &memStore{}
	stats, err := Run(t.Context(), store, Options{
		Days:     3,
		FirstDay: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Location: time.UTC,
	})
	if err != nil {
		t.Fatal(err)
	}

	if stats.Movies != len(movies) || stats.Halls != len(halls) {
		t.Errorf("stats = %+v", stats)
	}
	if stats.Seats != 138+96+40 {
		t.Errorf("seats = %d, want 274", stats.Seats)
	}
	if stats.Showtimes != len(store.showtimes) || stats.Showtimes < 3*len(halls)*4 {
		t.Errorf("showtimes = %d (store has %d)", stats.Showtimes, len(store.showtimes))
	}
	for _, s := range store.showtimes {
		if s.StartsAt.Before(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) ||
			!s.StartsAt.Before(time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("showtime at %s is outside the three seeded days", s.StartsAt)
		}
	}
	for _, m := range store.movies {
		if m.PosterURL != "" {
			t.Errorf("movie %q got poster %q without a template", m.Title, m.PosterURL)
		}
	}
}

func TestRunGivesPostersFromTheTemplate(t *testing.T) {
	t.Parallel()

	store := &memStore{}
	_, err := Run(t.Context(), store, Options{
		Days: 1, FirstDay: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Location: time.UTC,
		PosterURLTemplate: "https://picsum.photos/seed/{slug}/400/600",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.movies) != len(movies) {
		t.Fatalf("%d movies created, want %d", len(store.movies), len(movies))
	}
	seen := map[string]bool{}
	for _, m := range store.movies {
		want := "https://picsum.photos/seed/" + Slug(m.Title) + "/400/600"
		if m.PosterURL != want {
			t.Errorf("poster of %q = %q, want %q", m.Title, m.PosterURL, want)
		}
		if seen[m.PosterURL] {
			t.Errorf("two movies share the poster %q", m.PosterURL)
		}
		seen[m.PosterURL] = true
	}
	if store.movies[0].PosterURL != "https://picsum.photos/seed/the-last-projectionist/400/600" {
		t.Errorf("first poster = %q", store.movies[0].PosterURL)
	}
}

func TestRunRejectsAnInvalidPosterURL(t *testing.T) {
	t.Parallel()

	store := &memStore{}
	_, err := Run(t.Context(), store, Options{
		Days: 1, FirstDay: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Location: time.UTC,
		PosterURLTemplate: "https://img.example/{slug}/" + strings.Repeat("x", domain.MaxPosterURLLength),
	})
	if err == nil || !strings.Contains(err.Error(), "poster_url") {
		t.Fatalf("err = %v, want the poster_url rule", err)
	}
	if len(store.movies) != 0 {
		t.Errorf("%d movies created before the error", len(store.movies))
	}
}

func TestSlug(t *testing.T) {
	t.Parallel()

	for title, want := range map[string]string{
		"The Last Projectionist":  "the-last-projectionist",
		"Salt and Thunder":        "salt-and-thunder",
		"  Dune: Part Two (2024)": "dune-part-two-2024",
		"Amélie":                  "am-lie",
		"WALL·E":                  "wall-e",
		"!!!":                     "",
	} {
		if got := Slug(title); got != want {
			t.Errorf("Slug(%q) = %q, want %q", title, got, want)
		}
	}
}
