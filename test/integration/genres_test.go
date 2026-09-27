//go:build integration

package integration

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onetodone/cinema-api/internal/domain"
)

// fieldsOf returns the fields a *domain.ValidationError names, or nil for any other error.
func fieldsOf(err error) []string {
	var ve *domain.ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	fields := make([]string, len(ve.Fields))
	for i, f := range ve.Fields {
		fields[i] = f.Field
	}
	return fields
}

// TestGenreCatalogCRUD creates, reads, renames, and deletes genres through the repository, with the unique slug and
// name and the foreign key that protects genres in use.
func TestGenreCatalogCRUD(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()

	all := must(f.catalog.ListGenres(ctx))(t)
	if len(all) != 18 || all[0].Slug != "action" || all[len(all)-1].Slug != "western" {
		t.Fatalf("initial genres = %v, want the 18 from the migration", all)
	}
	if !slices.IsSortedFunc(all, func(a, b domain.Genre) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) }) {
		t.Errorf("genres are not ordered by name: %v", all)
	}

	noir := must(f.catalog.CreateGenre(ctx, domain.NewGenre{Slug: "noir", Name: "Noir"}))(t)
	if noir.ID == 0 || noir.Slug != "noir" || noir.Name != "Noir" {
		t.Errorf("created %+v", noir)
	}
	if got := must(f.catalog.GetGenre(ctx, noir.ID))(t); got != noir {
		t.Errorf("read %+v, want %+v", got, noir)
	}

	for _, tt := range []struct {
		genre domain.NewGenre
		code  string
	}{
		{genre: domain.NewGenre{Slug: "drama", Name: "Drama 2"}, code: domain.CodeGenreSlugTaken},
		{genre: domain.NewGenre{Slug: "drama_2", Name: "DRAMA"}, code: domain.CodeGenreNameTaken},
	} {
		if _, err := f.catalog.CreateGenre(ctx, tt.genre); domainCode(err) != tt.code {
			t.Errorf("create %+v = %v, want %s", tt.genre, err, tt.code)
		}
		if _, err := f.catalog.UpdateGenre(ctx, noir.ID, tt.genre); domainCode(err) != tt.code {
			t.Errorf("update to %+v = %v, want %s", tt.genre, err, tt.code)
		}
	}

	renamed := must(f.catalog.UpdateGenre(ctx, noir.ID, domain.NewGenre{Slug: "film_noir", Name: "Film noir"}))(t)
	if renamed != (domain.Genre{ID: noir.ID, Slug: "film_noir", Name: "Film noir"}) {
		t.Errorf("renamed = %+v", renamed)
	}
	// Changing only the case of its own name is not a conflict with itself.
	must(f.catalog.UpdateGenre(ctx, noir.ID, domain.NewGenre{Slug: "film_noir", Name: "Film Noir"}))(t)
	if _, err := f.catalog.UpdateGenre(ctx, 999_999, domain.NewGenre{Slug: "x", Name: "X"}); domainCode(err) != domain.CodeGenreNotFound {
		t.Errorf("update of an unknown genre = %v", err)
	}
	if _, err := f.catalog.GetGenre(ctx, 999_999); domainCode(err) != domain.CodeGenreNotFound {
		t.Errorf("read of an unknown genre = %v", err)
	}

	drama := f.genres(t, "drama")[0]
	must(f.catalog.SetMovieGenres(ctx, f.dune.ID, []int64{drama.ID}))(t)
	if err := f.catalog.DeleteGenre(ctx, drama.ID); domainCode(err) != domain.CodeGenreInUse {
		t.Errorf("delete of a genre in use = %v, want GENRE_IN_USE", err)
	}
	if err := f.catalog.DeleteGenre(ctx, noir.ID); err != nil {
		t.Errorf("delete of an unused genre: %v", err)
	}
	if err := f.catalog.DeleteGenre(ctx, noir.ID); domainCode(err) != domain.CodeGenreNotFound {
		t.Errorf("second delete = %v, want GENRE_NOT_FOUND", err)
	}

	// The database refuses what the domain refuses, on its own.
	for _, slug := range []string{"Drama", "sci-fi", "a__b", ""} {
		_, err := f.pool.Exec(ctx, `INSERT INTO genres (slug, name) VALUES ($1, $1 || ' genre')`, slug)
		if pgCode(err) != "23514" {
			t.Errorf("slug %q = %v, want a check violation", slug, err)
		}
	}
}

// TestSetMovieGenres replaces a movie's genres and reads them back, in order, from every query that shows them.
func TestSetMovieGenres(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	g := f.genres(t, "thriller", "drama", "crime")

	m := must(f.catalog.SetMovieGenres(ctx, f.dune.ID, genreIDs(g)))(t)
	if !slices.Equal(m.Genres, g) {
		t.Errorf("set genres = %v, want %v in that order", m.Genres, g)
	}
	m = must(f.catalog.SetMovieGenres(ctx, f.dune.ID, []int64{g[2].ID, g[0].ID}))(t)
	if want := []domain.Genre{g[2], g[0]}; !slices.Equal(m.Genres, want) {
		t.Errorf("reordered genres = %v, want %v", m.Genres, want)
	}

	// A list with an unknown id changes nothing.
	_, err := f.catalog.SetMovieGenres(ctx, f.dune.ID, []int64{g[1].ID, 999_999})
	if got := fieldsOf(err); !slices.Equal(got, []string{"genre_ids[1]"}) {
		t.Errorf("unknown genre = %v, want a validation error on genre_ids[1]", err)
	}
	if got := must(f.catalog.GetMovie(ctx, f.dune.ID))(t); !slices.Equal(got.Genres, []domain.Genre{g[2], g[0]}) {
		t.Errorf("genres after a refused change = %v", got.Genres)
	}
	if _, err := f.catalog.SetMovieGenres(ctx, 999_999, nil); domainCode(err) != domain.CodeMovieNotFound {
		t.Errorf("unknown movie = %v, want MOVIE_NOT_FOUND", err)
	}

	// A rename shows everywhere the movie does.
	must(f.catalog.UpdateGenre(ctx, g[2].ID, domain.NewGenre{Slug: "heist", Name: "Heist"}))(t)
	heist := domain.Genre{ID: g[2].ID, Slug: "heist", Name: "Heist"}
	st := f.showtime(t, f.dune, f.hall, base)
	if got := must(f.catalog.GetShowtime(ctx, st.ID))(t); !slices.Equal(got.Movie.Genres, []domain.Genre{heist, g[0]}) {
		t.Errorf("showtime movie genres = %v", got.Movie.Genres)
	}
	if list := must(f.catalog.ListMovies(ctx, 0, 10))(t); !slices.Equal(list[0].Genres, []domain.Genre{heist, g[0]}) {
		t.Errorf("movie list genres = %v", list[0].Genres)
	}

	cleared := must(f.catalog.SetMovieGenres(ctx, f.dune.ID, nil))(t)
	if cleared.Genres == nil || len(cleared.Genres) != 0 {
		t.Errorf("cleared genres = %#v, want empty, not nil", cleared.Genres)
	}
	if n := countRows(t, f.pool, `SELECT count(*) FROM movie_genres`); n != 0 {
		t.Errorf("%d movie_genres rows left", n)
	}
}

// TestConcurrentMovieGenreReplacements replaces one movie's genres from many goroutines at once. Every replacement
// succeeds, and the movie ends up with exactly one of the requested lists, in its order.
func TestConcurrentMovieGenreReplacements(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	all := must(f.catalog.ListGenres(ctx))(t)

	const writers = 20
	lists := make([][]int64, writers)
	for i := range lists {
		n := 1 + i%domain.MaxMovieGenres
		for j := range n {
			lists[i] = append(lists[i], all[(i+j*3)%len(all)].ID)
		}
	}

	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make(chan error, writers)
	)
	for _, ids := range lists {
		wg.Go(func() {
			<-start
			_, err := f.catalog.SetMovieGenres(ctx, f.dune.ID, ids)
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("replacement failed: %v", err)
		}
	}

	final := genreIDs(must(f.catalog.GetMovie(ctx, f.dune.ID))(t).Genres)
	if !slices.ContainsFunc(lists, func(ids []int64) bool { return slices.Equal(ids, final) }) {
		t.Errorf("final genres %v are none of the requested lists", final)
	}
	if n := countRows(t, f.pool, `
SELECT count(*) FROM movie_genres WHERE movie_id = $1 AND position BETWEEN 1 AND $2`, f.dune.ID, len(final)); n != len(final) {
		t.Errorf("%d rows at positions 1..%d, want %d", n, len(final), len(final))
	}
}

// TestGenreDeleteRacesAssignment deletes a genre while it is assigned to a movie. Either the assignment wins and
// the delete is refused as in use, or the delete wins and the assignment reports the id unknown; never both, never
// a server error, and never a movie with a deleted genre.
func TestGenreDeleteRacesAssignment(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()

	const rounds = 30
	var assigned, deleted int
	for i := range rounds {
		g := must(f.catalog.CreateGenre(ctx, domain.NewGenre{Slug: fmt.Sprintf("race_%d", i), Name: fmt.Sprintf("Race %d", i)}))(t)
		var (
			wg                   sync.WaitGroup
			start                = make(chan struct{})
			assignErr, deleteErr error
		)
		wg.Go(func() {
			<-start
			_, assignErr = f.catalog.SetMovieGenres(ctx, f.dune.ID, []int64{g.ID})
		})
		wg.Go(func() {
			<-start
			deleteErr = f.catalog.DeleteGenre(ctx, g.ID)
		})
		close(start)
		wg.Wait()

		switch {
		case assignErr == nil && domainCode(deleteErr) == domain.CodeGenreInUse:
			assigned++
			// Free the genre's movie for the next round.
			must(f.catalog.SetMovieGenres(ctx, f.dune.ID, nil))(t)
		case deleteErr == nil && slices.Equal(fieldsOf(assignErr), []string{"genre_ids[0]"}):
			deleted++
		default:
			t.Fatalf("round %d: assign = %v, delete = %v", i, assignErr, deleteErr)
		}
		if n := countRows(t, f.pool, `
SELECT count(*) FROM movie_genres mg WHERE NOT EXISTS (SELECT 1 FROM genres g WHERE g.id = mg.genre_id)`); n != 0 {
			t.Fatalf("round %d: %d movie_genres rows without a genre", i, n)
		}
	}
	t.Logf("assignment won %d rounds, delete won %d", assigned, deleted)
}

// TestAssignmentWaitsForAnUncommittedGenreDelete assigns a genre whose delete has not committed yet. The
// assignment must wait for the delete and then report the id unknown; reading the genre without a lock would let
// it insert a row that the committed delete leaves without a genre, which the foreign key turns into a 500.
func TestAssignmentWaitsForAnUncommittedGenreDelete(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	g := must(f.catalog.CreateGenre(ctx, domain.NewGenre{Slug: "doomed", Name: "Doomed"}))(t)

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM genres WHERE id = $1`, g.ID); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := f.catalog.SetMovieGenres(ctx, f.dune.ID, []int64{g.ID})
		done <- err
	}()
	// Wait until the assignment blocks on the deleted row, then commit the delete.
	const waiting = `
SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`
	for countRows(t, f.pool, waiting) == 0 {
		select {
		case err := <-done:
			t.Fatalf("the assignment did not wait for the delete: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if err := <-done; !slices.Equal(fieldsOf(err), []string{"genre_ids[0]"}) {
		t.Errorf("assignment = %v, want a validation error on genre_ids[0]", err)
	}
	if n := countRows(t, f.pool, `SELECT count(*) FROM movie_genres`); n != 0 {
		t.Errorf("%d movie_genres rows, want none", n)
	}
}

// TestAdminAPIGenreCatalog manages genres and a movie's genres through the API, as an admin would.
func TestAdminAPIGenreCatalog(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	api := newAPIServer(t, f)
	adminToken := api.signInAdmin(f)
	customer := api.signUp("ann@example.com")

	list := api.do(http.MethodGet, "/v1/genres", "", nil)
	etag := list.header.Get("ETag")
	if items, _ := list.body["items"].([]any); list.status != http.StatusOK || len(items) != 18 || etag == "" {
		t.Fatalf("genre list = %d %v, ETag %q", list.status, list.body, etag)
	}

	if r := api.do(http.MethodPost, "/v1/admin/genres", customer, map[string]any{"slug": "noir", "name": "Noir"}); r.status != http.StatusForbidden {
		t.Errorf("a customer creating a genre = %d", r.status)
	}
	created := api.do(http.MethodPost, "/v1/admin/genres", adminToken, map[string]any{"slug": " Film_Noir ", "name": " Film noir "})
	genreID, _ := created.body["id"].(float64)
	if created.status != http.StatusCreated || created.body["slug"] != "film_noir" || created.body["name"] != "Film noir" {
		t.Fatalf("create genre = %d %v", created.status, created.body)
	}
	location := created.header.Get("Location")
	if location != fmt.Sprintf("/v1/genres/%d", int64(genreID)) {
		t.Errorf("Location = %q", location)
	}
	if got := api.do(http.MethodGet, location, "", nil); got.status != http.StatusOK || !reflect.DeepEqual(got.body, created.body) {
		t.Errorf("read genre = %d %v, want %v", got.status, got.body, created.body)
	}
	changed := api.doWith(http.MethodGet, "/v1/genres", "", nil, http.Header{"If-None-Match": {etag}})
	if items, _ := changed.body["items"].([]any); changed.status != http.StatusOK || len(items) != 19 {
		t.Errorf("list after a create with the old ETag = %d with %d genres, want 200 with 19", changed.status, len(items))
	}

	for _, tt := range []struct {
		body   map[string]any
		status int
		code   string
	}{
		{body: map[string]any{"slug": "drama", "name": "Noir 2"}, status: http.StatusConflict, code: domain.CodeGenreSlugTaken},
		{body: map[string]any{"slug": "noir_2", "name": "FILM NOIR"}, status: http.StatusConflict, code: domain.CodeGenreNameTaken},
		{body: map[string]any{"slug": "sci-fi", "name": ""}, status: http.StatusBadRequest, code: "VALIDATION_FAILED"},
	} {
		r := api.do(http.MethodPost, "/v1/admin/genres", adminToken, tt.body)
		if r.status != tt.status || r.body["code"] != tt.code {
			t.Errorf("create %v = %d %v, want %d %s", tt.body, r.status, r.body["code"], tt.status, tt.code)
		}
	}

	updated := api.do(http.MethodPut, fmt.Sprintf("/v1/admin/genres/%d", int64(genreID)), adminToken, map[string]any{"slug": "noir", "name": "Noir"})
	if updated.status != http.StatusOK || updated.body["slug"] != "noir" || updated.body["name"] != "Noir" {
		t.Errorf("update genre = %d %v", updated.status, updated.body)
	}
	if r := api.do(http.MethodPut, "/v1/admin/genres/999999", adminToken, map[string]any{"slug": "x", "name": "X"}); r.status != http.StatusNotFound {
		t.Errorf("update of an unknown genre = %d %v", r.status, r.body)
	}

	// Assign it with drama to a movie; the movie page shows both, in order, with the new name.
	drama := f.genres(t, "drama")[0]
	moviePath := fmt.Sprintf("/v1/admin/movies/%d/genres", f.dune.ID)
	set := api.do(http.MethodPut, moviePath, adminToken, map[string]any{"genre_ids": []any{genreID, drama.ID}})
	want := []any{
		map[string]any{"id": genreID, "slug": "noir", "name": "Noir"},
		genresJSON(drama)[0],
	}
	if set.status != http.StatusOK || !reflect.DeepEqual(set.body["genres"], want) {
		t.Fatalf("set movie genres = %d %v", set.status, set.body)
	}
	if page := api.do(http.MethodGet, fmt.Sprintf("/v1/movies/%d", f.dune.ID), "", nil); !reflect.DeepEqual(page.body["genres"], want) {
		t.Errorf("movie page genres = %v, want %v", page.body["genres"], want)
	}
	if r := api.do(http.MethodPut, moviePath, adminToken, map[string]any{}); r.status != http.StatusBadRequest ||
		!slices.Equal(errorFields(r), []string{"genre_ids"}) {
		t.Errorf("set without genre_ids = %d %v", r.status, r.body)
	}
	if r := api.do(http.MethodPut, "/v1/admin/movies/999999/genres", adminToken, map[string]any{"genre_ids": []any{}}); r.status != http.StatusNotFound {
		t.Errorf("genres of an unknown movie = %d %v", r.status, r.body)
	}

	genrePath := fmt.Sprintf("/v1/admin/genres/%d", int64(genreID))
	if r := api.do(http.MethodDelete, genrePath, adminToken, nil); r.status != http.StatusConflict || r.body["code"] != domain.CodeGenreInUse {
		t.Errorf("delete of a genre in use = %d %v", r.status, r.body)
	}
	if r := api.do(http.MethodPut, moviePath, adminToken, map[string]any{"genre_ids": []any{}}); r.status != http.StatusOK ||
		!reflect.DeepEqual(r.body["genres"], []any{}) {
		t.Errorf("clearing the genres = %d %v", r.status, r.body)
	}
	if r := api.do(http.MethodDelete, genrePath, adminToken, nil); r.status != http.StatusNoContent {
		t.Errorf("delete = %d %v", r.status, r.body)
	}
	if r := api.do(http.MethodGet, location, "", nil); r.status != http.StatusNotFound || r.body["code"] != domain.CodeGenreNotFound {
		t.Errorf("read after delete = %d %v", r.status, r.body)
	}
}
