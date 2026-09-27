package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/service/catalog"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

// stubCatalog records the arguments it receives and returns canned results.
type stubCatalog struct {
	afterID  int64
	limit    int
	schedule catalog.ScheduleQuery
	page     catalog.MoviePage
	details  catalog.MovieDetails
	sched    catalog.Schedule
	showtime domain.Showtime
	seatMap  catalog.SeatMap
	genres   []domain.Genre
	err      error
}

func (s *stubCatalog) ListMovies(_ context.Context, afterID int64, limit int) (catalog.MoviePage, error) {
	s.afterID, s.limit = afterID, limit
	return s.page, s.err
}

func (s *stubCatalog) GetMovie(context.Context, int64) (catalog.MovieDetails, error) {
	return s.details, s.err
}

func (s *stubCatalog) Schedule(_ context.Context, q catalog.ScheduleQuery) (catalog.Schedule, error) {
	s.schedule = q
	return s.sched, s.err
}

func (s *stubCatalog) GetShowtime(context.Context, int64) (domain.Showtime, error) {
	return s.showtime, s.err
}

func (s *stubCatalog) SeatMap(context.Context, int64) (catalog.SeatMap, error) {
	return s.seatMap, s.err
}

func (s *stubCatalog) ListGenres(context.Context) ([]domain.Genre, error) {
	return s.genres, s.err
}

func (s *stubCatalog) GetGenre(_ context.Context, id int64) (domain.Genre, error) {
	for _, g := range s.genres {
		if g.ID == id {
			return g, nil
		}
	}
	if s.err != nil {
		return domain.Genre{}, s.err
	}
	return domain.Genre{}, domain.NotFound(domain.CodeGenreNotFound, "genre %d not found", id)
}

func serveCatalog(t *testing.T, svc CatalogService, target string) *httptest.ResponseRecorder {
	t.Helper()
	return serveCatalogWith(t, svc, target, nil)
}

// serveCatalogWith is serveCatalog with request headers.
func serveCatalogWith(t *testing.T, svc CatalogService, target string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	h := NewCatalog(svc, "USD", slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/movies", h.ListMovies)
	mux.HandleFunc("GET /v1/movies/{movieID}", h.GetMovie)
	mux.HandleFunc("GET /v1/genres", h.ListGenres)
	mux.HandleFunc("GET /v1/genres/{genreID}", h.GetGenre)
	mux.HandleFunc("GET /v1/showtimes", h.Schedule)
	mux.HandleFunc("GET /v1/showtimes/{showtimeID}", h.GetShowtime)
	mux.HandleFunc("GET /v1/showtimes/{showtimeID}/seats", h.SeatMap)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) problem.Problem {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status = %d, want %d (body %s)", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != problem.ContentType {
		t.Errorf("Content-Type = %q, want %q", ct, problem.ContentType)
	}
	p := decode[problem.Problem](t, rec)
	if p.Code != code {
		t.Errorf("code = %q, want %q", p.Code, code)
	}
	return p
}

// Genres of the sample data.
var (
	genreSciFi     = domain.Genre{ID: 15, Slug: "science_fiction", Name: "Science fiction"}
	genreAdventure = domain.Genre{ID: 2, Slug: "adventure", Name: "Adventure"}
	genreDrama     = domain.Genre{ID: 7, Slug: "drama", Name: "Drama"}
)

// genresJSON is how genres decode from a response into map[string]any.
func genresJSON(genres ...domain.Genre) []any {
	out := make([]any, len(genres))
	for i, g := range genres {
		out[i] = map[string]any{"id": float64(g.ID), "slug": g.Slug, "name": g.Name}
	}
	return out
}

var sampleShowtime = domain.Showtime{
	ID: 11,
	Movie: domain.MovieSummary{
		ID: 1, Title: "Dune", DurationMin: 155, AgeRating: "PG-13",
		Genres: []domain.Genre{genreSciFi, genreAdventure},
	},
	Hall:           domain.Hall{ID: 2, Name: "Hall 2"},
	StartsAt:       time.Date(2026, 10, 1, 19, 0, 0, 0, time.UTC),
	EndsAt:         time.Date(2026, 10, 1, 21, 50, 0, 0, time.UTC),
	Language:       domain.LanguageVersion{Audio: "eng", Subtitles: "tha"},
	BasePriceCents: 1200,
	Status:         domain.ShowtimeScheduled,
	SeatsAvailable: 90,
	SeatsTotal:     100,
}

func TestListMoviesPassesPagingAndReturnsCursor(t *testing.T) {
	t.Parallel()

	svc := &stubCatalog{page: catalog.MoviePage{
		Movies:      []domain.Movie{{ID: 3, Title: "A", DurationMin: 90}, {ID: 4, Title: "B", DurationMin: 100}},
		NextAfterID: 4,
	}}
	rec := serveCatalog(t, svc, "/v1/movies?limit=2&cursor="+encodeCursor(2))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if svc.afterID != 2 || svc.limit != 2 {
		t.Errorf("service got afterID=%d limit=%d, want 2/2", svc.afterID, svc.limit)
	}
	body := decode[struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}](t, rec)
	if len(body.Items) != 2 || body.Items[0]["title"] != "A" {
		t.Errorf("items = %v", body.Items)
	}
	if id, err := decodeCursor(body.NextCursor); err != nil || id != 4 {
		t.Errorf("next_cursor %q decodes to %d, %v; want 4", body.NextCursor, id, err)
	}
}

func TestListMoviesLastPageHasEmptyListNotNull(t *testing.T) {
	t.Parallel()

	rec := serveCatalog(t, &stubCatalog{}, "/v1/movies")
	if got := strings.TrimSpace(rec.Body.String()); got != `{"items":[]}` {
		t.Errorf("body = %s, want {\"items\":[]}", got)
	}
}

func TestListMoviesRejectsBadParameters(t *testing.T) {
	t.Parallel()

	tests := map[string][]string{
		"/v1/movies?limit=0":                     {"limit"},
		"/v1/movies?limit=101":                   {"limit"},
		"/v1/movies?limit=ten":                   {"limit"},
		"/v1/movies?cursor=!!!":                  {"cursor"},
		"/v1/movies?cursor=" + encodeCursor(-5):  {"cursor"},
		"/v1/movies?limit=-1&cursor=not-base-64": {"limit", "cursor"},
	}
	for target, fields := range tests {
		t.Run(target, func(t *testing.T) {
			t.Parallel()

			svc := &stubCatalog{}
			p := assertProblem(t, serveCatalog(t, svc, target), http.StatusBadRequest, problem.CodeValidationFailed)
			if len(p.Errors) != len(fields) {
				t.Fatalf("errors = %+v, want fields %v", p.Errors, fields)
			}
			for i, f := range fields {
				if p.Errors[i].Field != f {
					t.Errorf("errors[%d].field = %q, want %q", i, p.Errors[i].Field, f)
				}
			}
		})
	}
}

func TestGetMovie(t *testing.T) {
	t.Parallel()

	svc := &stubCatalog{details: catalog.MovieDetails{
		Movie:    domain.Movie{ID: 1, Title: "Dune", DurationMin: 155, Genres: []domain.Genre{genreDrama}},
		Upcoming: []domain.Showtime{sampleShowtime},
	}}
	rec := serveCatalog(t, svc, "/v1/movies/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	body := decode[map[string]any](t, rec)
	if body["title"] != "Dune" || !reflect.DeepEqual(body["genres"], genresJSON(genreDrama)) {
		t.Errorf("title, genres = %v, %v", body["title"], body["genres"])
	}
	upcoming, _ := body["upcoming_showtimes"].([]any)
	if len(upcoming) != 1 {
		t.Fatalf("upcoming_showtimes = %v", body["upcoming_showtimes"])
	}
	first, _ := upcoming[0].(map[string]any)
	if first["currency"] != "USD" || first["starts_at"] != "2026-10-01T19:00:00Z" {
		t.Errorf("showtime = %v", first)
	}
}

func TestGetMovieErrors(t *testing.T) {
	t.Parallel()

	notFound := &stubCatalog{err: domain.NotFound(domain.CodeMovieNotFound, "movie 9 not found")}
	p := assertProblem(t, serveCatalog(t, notFound, "/v1/movies/9"), http.StatusNotFound, domain.CodeMovieNotFound)
	if p.Detail != "movie 9 not found" || p.Instance != "/v1/movies/9" {
		t.Errorf("problem = %+v", p)
	}

	assertProblem(t, serveCatalog(t, &stubCatalog{}, "/v1/movies/abc"), http.StatusBadRequest,
		problem.CodeValidationFailed)
	assertProblem(t, serveCatalog(t, &stubCatalog{}, "/v1/movies/0"), http.StatusBadRequest,
		problem.CodeValidationFailed)

	broken := &stubCatalog{err: errors.New("connection reset by peer")}
	rec := serveCatalog(t, broken, "/v1/movies/1")
	assertProblem(t, rec, http.StatusInternalServerError, problem.CodeInternal)
	if strings.Contains(rec.Body.String(), "connection reset") {
		t.Errorf("500 body leaks the internal error: %s", rec.Body.String())
	}
}

func TestSchedule(t *testing.T) {
	t.Parallel()

	svc := &stubCatalog{sched: catalog.Schedule{
		Day:       time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Showtimes: []domain.Showtime{sampleShowtime},
	}}
	rec := serveCatalog(t, svc, "/v1/showtimes?date=2026-10-01&movie_id=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	if svc.schedule.MovieID != 1 || svc.schedule.Day.Format(time.DateOnly) != "2026-10-01" {
		t.Errorf("service got %+v", svc.schedule)
	}
	body := decode[map[string]any](t, rec)
	if body["date"] != "2026-10-01" {
		t.Errorf("date = %v", body["date"])
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", body["items"])
	}
	item, _ := items[0].(map[string]any)
	for k, v := range map[string]any{
		"seats_available": 90.0, "seats_total": 100.0, "base_price_cents": 1200.0,
		"audio_language": "eng", "subtitle_language": "tha",
	} {
		if item[k] != v {
			t.Errorf("%s = %v, want %v", k, item[k], v)
		}
	}
	if movie, _ := item["movie"].(map[string]any); !reflect.DeepEqual(movie["genres"], genresJSON(genreSciFi, genreAdventure)) {
		t.Errorf("movie = %v, want its genres", item["movie"])
	}
}

func TestShowtimeWithoutSubtitlesOrGenres(t *testing.T) {
	t.Parallel()

	st := sampleShowtime
	st.Language.Subtitles, st.Movie.Genres = "", nil
	body := decode[map[string]any](t, serveCatalog(t, &stubCatalog{showtime: st}, "/v1/showtimes/11"))
	if _, ok := body["subtitle_language"]; ok || body["audio_language"] != "eng" {
		t.Errorf("body = %v, want audio_language eng and no subtitle_language", body)
	}
	if movie, _ := body["movie"].(map[string]any); !reflect.DeepEqual(movie["genres"], []any{}) {
		t.Errorf("movie = %v, want genres []", body["movie"])
	}
}

func TestScheduleWithoutDateAsksForToday(t *testing.T) {
	t.Parallel()

	svc := &stubCatalog{}
	if rec := serveCatalog(t, svc, "/v1/showtimes"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !svc.schedule.Day.IsZero() || svc.schedule.MovieID != 0 {
		t.Errorf("service got %+v, want the zero query", svc.schedule)
	}
}

func TestScheduleRejectsBadParameters(t *testing.T) {
	t.Parallel()

	for _, target := range []string{
		"/v1/showtimes?date=01-10-2026",
		"/v1/showtimes?date=2026-02-30",
		"/v1/showtimes?movie_id=x",
		"/v1/showtimes?movie_id=-3",
	} {
		assertProblem(t, serveCatalog(t, &stubCatalog{}, target), http.StatusBadRequest, problem.CodeValidationFailed)
	}
}

func TestGetShowtime(t *testing.T) {
	t.Parallel()

	rec := serveCatalog(t, &stubCatalog{showtime: sampleShowtime}, "/v1/showtimes/11")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decode[map[string]any](t, rec)
	if body["id"] != 11.0 || body["status"] != "scheduled" {
		t.Errorf("body = %v", body)
	}

	notFound := &stubCatalog{err: domain.NotFound(domain.CodeShowtimeNotFound, "showtime 5 not found")}
	assertProblem(t, serveCatalog(t, notFound, "/v1/showtimes/5"), http.StatusNotFound, domain.CodeShowtimeNotFound)
}

func TestSeatMap(t *testing.T) {
	t.Parallel()

	svc := &stubCatalog{seatMap: catalog.SeatMap{
		Showtime: sampleShowtime,
		Seats: []domain.ShowtimeSeat{
			{SeatID: 100, Row: "A", Number: 1, Type: domain.SeatStandard, PriceCents: 1200, Status: domain.SeatAvailable},
			{SeatID: 101, Row: "A", Number: 2, Type: domain.SeatVIP, PriceCents: 1800, Status: domain.SeatHeld},
		},
		Summary: catalog.SeatSummary{Available: 1, Held: 1, Total: 2},
	}}
	rec := serveCatalog(t, svc, "/v1/showtimes/11/seats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	body := decode[struct {
		ShowtimeID       int64            `json:"showtime_id"`
		AudioLanguage    string           `json:"audio_language"`
		SubtitleLanguage string           `json:"subtitle_language"`
		Currency         string           `json:"currency"`
		Summary          map[string]int   `json:"summary"`
		Seats            []map[string]any `json:"seats"`
	}](t, rec)
	if body.ShowtimeID != 11 || body.Currency != "USD" {
		t.Errorf("showtime_id/currency = %d/%s", body.ShowtimeID, body.Currency)
	}
	if body.AudioLanguage != "eng" || body.SubtitleLanguage != "tha" {
		t.Errorf("languages = %s/%s, want eng/tha", body.AudioLanguage, body.SubtitleLanguage)
	}
	if body.Summary["available"] != 1 || body.Summary["held"] != 1 || body.Summary["total"] != 2 {
		t.Errorf("summary = %v", body.Summary)
	}
	if len(body.Seats) != 2 || body.Seats[1]["status"] != "held" || body.Seats[1]["type"] != "vip" {
		t.Errorf("seats = %v", body.Seats)
	}
}

// TestPolledReadsAnswerNotModified covers the two routes that clients poll: an ETag and no-cache on every answer,
// and 304 without a body when If-None-Match names the current ETag.
func TestPolledReadsAnswerNotModified(t *testing.T) {
	t.Parallel()

	seatMap := func(held domain.SeatStatus) *stubCatalog {
		return &stubCatalog{seatMap: catalog.SeatMap{
			Showtime: sampleShowtime,
			Seats:    []domain.ShowtimeSeat{{SeatID: 100, Row: "A", Number: 1, Type: domain.SeatStandard, Status: held}},
			Summary:  catalog.SeatSummary{Available: 1, Total: 1},
		}}
	}
	schedule := func(available int) *stubCatalog {
		st := sampleShowtime
		st.SeatsAvailable = available
		return &stubCatalog{sched: catalog.Schedule{Day: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Showtimes: []domain.Showtime{st}}}
	}
	routes := []struct {
		name          string
		target        string
		before, after *stubCatalog // the data before and after a change
	}{
		{"seat map", "/v1/showtimes/11/seats", seatMap(domain.SeatAvailable), seatMap(domain.SeatHeld)},
		{"schedule", "/v1/showtimes?date=2026-10-01", schedule(90), schedule(89)},
	}
	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			t.Parallel()

			first := serveCatalog(t, rt.before, rt.target)
			etag := first.Header().Get("ETag")
			if first.Code != http.StatusOK || !strings.HasPrefix(etag, `W/"`) || len(etag) != len(`W/""`)+43 {
				t.Fatalf("status %d, ETag %q; want 200 and a weak SHA-256 tag", first.Code, etag)
			}
			if cc := first.Header().Get("Cache-Control"); cc != "no-cache" {
				t.Errorf("Cache-Control = %q, want no-cache", cc)
			}
			if again := serveCatalog(t, rt.before, rt.target); again.Header().Get("ETag") != etag {
				t.Errorf("the same content got ETag %q, then %q", etag, again.Header().Get("ETag"))
			}

			unchanged := serveCatalogWith(t, rt.before, rt.target, http.Header{"If-None-Match": {etag}})
			if unchanged.Code != http.StatusNotModified || unchanged.Body.Len() != 0 {
				t.Errorf("If-None-Match with the current ETag = %d with %d bytes, want 304 without a body",
					unchanged.Code, unchanged.Body.Len())
			}
			if unchanged.Header().Get("ETag") != etag || unchanged.Header().Get("Cache-Control") != "no-cache" {
				t.Errorf("304 headers = %v, want the ETag and Cache-Control of a 200", unchanged.Header())
			}

			changed := serveCatalogWith(t, rt.after, rt.target, http.Header{"If-None-Match": {etag}})
			if changed.Code != http.StatusOK || changed.Header().Get("ETag") == etag || changed.Body.Len() == 0 {
				t.Errorf("after a change: %d, ETag %q; want 200 with a new ETag and the body", changed.Code, changed.Header().Get("ETag"))
			}
		})
	}

	// Errors carry no ETag, whatever the request says.
	notFound := &stubCatalog{err: domain.NotFound(domain.CodeShowtimeNotFound, "showtime 5 not found")}
	rec := serveCatalogWith(t, notFound, "/v1/showtimes/5/seats", http.Header{"If-None-Match": {"*"}})
	assertProblem(t, rec, http.StatusNotFound, domain.CodeShowtimeNotFound)
	if rec.Header().Get("ETag") != "" {
		t.Errorf("a problem carries ETag %q", rec.Header().Get("ETag"))
	}
}

func TestListGenres(t *testing.T) {
	t.Parallel()

	svc := &stubCatalog{genres: []domain.Genre{genreAdventure, genreDrama, genreSciFi}}
	rec := serveCatalog(t, svc, "/v1/genres")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"items":[{"id":2,"slug":"adventure","name":"Adventure"},`+
		`{"id":7,"slug":"drama","name":"Drama"},{"id":15,"slug":"science_fiction","name":"Science fiction"}]}` {
		t.Errorf("body = %s", got)
	}

	// Clients revalidate the list; a changed list gets a new tag.
	etag := rec.Header().Get("ETag")
	again := serveCatalogWith(t, svc, "/v1/genres", http.Header{"If-None-Match": {etag}})
	if etag == "" || again.Code != http.StatusNotModified {
		t.Errorf("ETag %q, then %d; want a tag and 304", etag, again.Code)
	}
	renamed := &stubCatalog{genres: []domain.Genre{genreAdventure, {ID: 7, Slug: "drama", Name: "Dramas"}, genreSciFi}}
	if changed := serveCatalogWith(t, renamed, "/v1/genres", http.Header{"If-None-Match": {etag}}); changed.Code != http.StatusOK {
		t.Errorf("after a rename: %d, want 200", changed.Code)
	}

	// No genres: an empty list, not null.
	if got := strings.TrimSpace(serveCatalog(t, &stubCatalog{}, "/v1/genres").Body.String()); got != `{"items":[]}` {
		t.Errorf("empty list = %s", got)
	}
	assertProblem(t, serveCatalog(t, &stubCatalog{err: errors.New("db down")}, "/v1/genres"),
		http.StatusInternalServerError, problem.CodeInternal)
}

func TestGetGenre(t *testing.T) {
	t.Parallel()

	svc := &stubCatalog{genres: []domain.Genre{genreDrama, genreSciFi}}
	rec := serveCatalog(t, svc, "/v1/genres/15")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"id":15,"slug":"science_fiction","name":"Science fiction"}` {
		t.Errorf("body = %s", got)
	}
	assertProblem(t, serveCatalog(t, svc, "/v1/genres/99"), http.StatusNotFound, domain.CodeGenreNotFound)
	assertProblem(t, serveCatalog(t, svc, "/v1/genres/drama"), http.StatusBadRequest, problem.CodeValidationFailed)
}
