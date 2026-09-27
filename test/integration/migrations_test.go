//go:build integration

package integration

import (
	"slices"
	"testing"
	"time"

	"github.com/onetodone/cinema-api/internal/platform/dbmigrate"
)

// TestMigrationsRoundTrip applies every migration, rolls all of them back, and applies them again. It proves
// that each Down section really undoes its Up section.
func TestMigrationsRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newEmptyDB(t)

	m, err := dbmigrate.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	tables := func() int {
		var n int
		err := pool.QueryRow(ctx, `
SELECT count(*) FROM information_schema.tables
WHERE table_schema = 'public' AND table_name <> 'goose_db_version'`).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	if _, err := m.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	const wantTables = 12
	if got := tables(); got != wantTables {
		t.Fatalf("after up: %d tables, want %d", got, wantTables)
	}

	status, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range status {
		if s.AppliedAt.IsZero() {
			t.Errorf("migration %d is not applied", s.Source.Version)
		}
	}

	if _, err := m.DownTo(ctx, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
	if got := tables(); got != 0 {
		t.Fatalf("after down: %d tables left, want 0", got)
	}
	var types int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE n.nspname = 'public' AND t.typtype = 'e'`).Scan(&types); err != nil {
		t.Fatal(err)
	}
	if types != 0 {
		t.Errorf("after down: %d enum types left, want 0", types)
	}

	if _, err := m.Up(ctx); err != nil {
		t.Fatalf("second up: %v", err)
	}
	if got := tables(); got != wantTables {
		t.Fatalf("after second up: %d tables, want %d", got, wantTables)
	}
}

// TestGenresLanguagesMigrationBackfills applies 00003 to a database that already has a showtime: the showtime gets
// English audio and no subtitles, the movie no genres, and new showtimes must state their audio.
func TestGenresLanguagesMigrationBackfills(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newEmptyDB(t)

	m, err := dbmigrate.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if _, err := m.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	if _, err := m.DownTo(ctx, 2); err != nil {
		t.Fatalf("down to 2: %v", err)
	}

	exec(t, pool, `INSERT INTO movies (title, duration_min) VALUES ('Old', 90)`)
	exec(t, pool, `INSERT INTO halls (name) VALUES ('Old hall')`)
	oldShowtime := `
INSERT INTO showtimes (movie_id, hall_id, starts_at, ends_at, base_price_cents)
SELECT m.id, h.id, $1::timestamptz, $1::timestamptz + interval '2 hours', 900 FROM movies m, halls h`
	exec(t, pool, oldShowtime, base)

	if _, err := m.UpTo(ctx, 3); err != nil {
		t.Fatalf("up to 3: %v", err)
	}
	var (
		audio     string
		subtitles *string
		genres    []string
	)
	err = pool.QueryRow(ctx, `
SELECT s.audio_language, s.subtitle_language, m.genres::text[] FROM showtimes s JOIN movies m ON m.id = s.movie_id`).
		Scan(&audio, &subtitles, &genres)
	if err != nil {
		t.Fatal(err)
	}
	if audio != "eng" || subtitles != nil || genres == nil || len(genres) != 0 {
		t.Errorf("backfilled showtime = %q, %v, genres %#v; want eng, no subtitles, no genres", audio, subtitles, genres)
	}

	_, err = pool.Exec(ctx, oldShowtime, base.Add(24*time.Hour))
	if pgCode(err) != "23502" { // not_null_violation: the backfill default is gone
		t.Errorf("a showtime without audio = %v, want a not-null violation", err)
	}
}

// TestGenreCatalogMigrationMovesGenres applies 00004 to movies with genres from the old enum: each keeps its
// genres, in order, as rows of the new catalog. Down moves them back, except for genres the enum does not know.
func TestGenreCatalogMigrationMovesGenres(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newEmptyDB(t)

	m, err := dbmigrate.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if _, err := m.UpTo(ctx, 3); err != nil {
		t.Fatalf("up to 3: %v", err)
	}
	exec(t, pool, `
INSERT INTO movies (title, duration_min, genres) VALUES
    ('Midnight Matinee', 96, '{horror,comedy}'),
    ('Orbit of Glass', 142, '{science_fiction,thriller,drama}'),
    ('Untagged', 90, '{}')`)

	if _, err := m.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	genresOf := func(title string) []string {
		t.Helper()
		var names []string
		err := pool.QueryRow(ctx, `
SELECT coalesce(array_agg(g.slug || ':' || g.name ORDER BY mg.position), '{}')
FROM movie_genres mg
JOIN genres g ON g.id = mg.genre_id
JOIN movies m ON m.id = mg.movie_id
WHERE m.title = $1`, title).Scan(&names)
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	if got := genresOf("Midnight Matinee"); !slices.Equal(got, []string{"horror:Horror", "comedy:Comedy"}) {
		t.Errorf("Midnight Matinee = %v", got)
	}
	if got := genresOf("Orbit of Glass"); !slices.Equal(got, []string{"science_fiction:Science fiction", "thriller:Thriller", "drama:Drama"}) {
		t.Errorf("Orbit of Glass = %v", got)
	}
	if got := genresOf("Untagged"); len(got) != 0 {
		t.Errorf("Untagged = %v, want no genres", got)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM genres`); n != 18 {
		t.Errorf("%d genres, want the 18 of the old enum", n)
	}

	// An admin adds a genre the old enum does not know; it cannot move back.
	exec(t, pool, `INSERT INTO genres (slug, name) VALUES ('noir', 'Noir')`)
	exec(t, pool, `
INSERT INTO movie_genres (movie_id, genre_id, position)
SELECT m.id, g.id, 3 FROM movies m, genres g WHERE m.title = 'Midnight Matinee' AND g.slug = 'noir'`)

	if _, err := m.DownTo(ctx, 3); err != nil {
		t.Fatalf("down to 3: %v", err)
	}
	var back []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(array_to_string(genres, ',') ORDER BY id) FROM movies`).Scan(&back); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(back, []string{"horror,comedy", "science_fiction,thriller,drama", ""}) {
		t.Errorf("genres after down = %q", back)
	}
}
