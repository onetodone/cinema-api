package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

// stubAdmin records what it receives.
type stubAdmin struct {
	movie    domain.NewMovie
	hall     domain.NewHall
	showtime domain.NewShowtime
	genre    domain.NewGenre
	genreID  int64
	movieID  int64
	genreIDs []int64
	calls    int
	err      error
}

func (s *stubAdmin) CreateMovie(_ context.Context, m domain.NewMovie) (domain.Movie, error) {
	s.calls++
	s.movie = m
	if s.err != nil {
		return domain.Movie{}, s.err
	}
	return domain.Movie{
		ID: 42, Title: m.Title, DurationMin: m.DurationMin, AgeRating: m.AgeRating, Genres: stubGenres(m.GenreIDs),
	}, nil
}

// stubGenres makes up a genre for each id.
func stubGenres(ids []int64) []domain.Genre {
	var genres []domain.Genre // nil for none, as a mapper must cope with
	for _, id := range ids {
		genres = append(genres, domain.Genre{ID: id, Slug: "g" + strconv.FormatInt(id, 10), Name: "G" + strconv.FormatInt(id, 10)})
	}
	return genres
}

func (s *stubAdmin) SetMovieGenres(_ context.Context, movieID int64, genreIDs []int64) (domain.Movie, error) {
	s.calls++
	s.movieID, s.genreIDs = movieID, genreIDs
	if s.err != nil {
		return domain.Movie{}, s.err
	}
	return domain.Movie{ID: movieID, Title: "Dune", DurationMin: 155, Genres: stubGenres(genreIDs)}, nil
}

func (s *stubAdmin) CreateGenre(_ context.Context, g domain.NewGenre) (domain.Genre, error) {
	s.calls++
	s.genre = g
	if s.err != nil {
		return domain.Genre{}, s.err
	}
	return domain.Genre{ID: 19, Slug: g.Slug, Name: g.Name}, nil
}

func (s *stubAdmin) UpdateGenre(_ context.Context, id int64, g domain.NewGenre) (domain.Genre, error) {
	s.calls++
	s.genreID, s.genre = id, g
	if s.err != nil {
		return domain.Genre{}, s.err
	}
	return domain.Genre{ID: id, Slug: g.Slug, Name: g.Name}, nil
}

func (s *stubAdmin) DeleteGenre(_ context.Context, id int64) error {
	s.calls++
	s.genreID = id
	return s.err
}

func (s *stubAdmin) CreateHall(_ context.Context, h domain.NewHall) (domain.HallLayout, error) {
	s.calls++
	s.hall = h
	if s.err != nil {
		return domain.HallLayout{}, s.err
	}
	return domain.HallLayout{
		Hall: domain.Hall{ID: 4, Name: h.Name},
		Seats: []domain.HallSeat{
			{ID: 301, Row: "A", Number: 1, Type: domain.SeatStandard},
			{ID: 302, Row: "A", Number: 2, Type: domain.SeatStandard},
			{ID: 303, Row: "B", Number: 1, Type: domain.SeatVIP},
		},
	}, nil
}

func (s *stubAdmin) CreateShowtime(_ context.Context, ns domain.NewShowtime) (domain.Showtime, error) {
	s.calls++
	s.showtime = ns
	if s.err != nil {
		return domain.Showtime{}, s.err
	}
	gst := time.FixedZone("GST", 4*3600)
	return domain.Showtime{
		ID: 77, Movie: domain.MovieSummary{ID: ns.MovieID, Title: "Dune", DurationMin: 155},
		Hall: domain.Hall{ID: ns.HallID, Name: "Hall 4"}, StartsAt: ns.StartsAt.In(gst),
		EndsAt: ns.StartsAt.Add(170 * time.Minute).In(gst), Language: ns.Language, BasePriceCents: ns.BasePriceCents,
		Status: domain.ShowtimeScheduled, SeatsAvailable: 3, SeatsTotal: 3,
	}, nil
}

func newAdminHandler(svc AdminService) *Admin {
	return NewAdmin(svc, "EUR", slog.New(slog.DiscardHandler))
}

// serveAdmin sends a request, with a JSON body unless body is empty, through the routes that have path parameters.
func serveAdmin(t *testing.T, svc AdminService, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := newAdminHandler(svc)
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/admin/movies/{movieID}/genres", h.SetMovieGenres)
	mux.HandleFunc("POST /v1/admin/genres", h.CreateGenre)
	mux.HandleFunc("PUT /v1/admin/genres/{genreID}", h.UpdateGenre)
	mux.HandleFunc("DELETE /v1/admin/genres/{genreID}", h.DeleteGenre)

	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestCreateMovie(t *testing.T) {
	t.Parallel()

	svc := &stubAdmin{}
	rec := postJSON(t, newAdminHandler(svc).CreateMovie, "/v1/admin/movies",
		`{"title":"Dune","description":"Sand.","duration_min":155,"age_rating":"PG-13","poster_url":"https://x/y.jpg",
		  "genre_ids":[15,2]}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/movies/42" {
		t.Errorf("Location = %q, want /v1/movies/42", loc)
	}
	want := domain.NewMovie{
		Title: "Dune", Description: "Sand.", DurationMin: 155, AgeRating: "PG-13", PosterURL: "https://x/y.jpg",
		GenreIDs: []int64{15, 2},
	}
	if !reflect.DeepEqual(svc.movie, want) {
		t.Errorf("service got %+v, want %+v", svc.movie, want)
	}
	body := decode[map[string]any](t, rec)
	if body["id"] != 42.0 || body["title"] != "Dune" || !reflect.DeepEqual(body["genres"], genresJSON(stubGenres([]int64{15, 2})...)) {
		t.Errorf("body = %v", body)
	}
}

func TestCreateMovieWithoutGenresAnswersAnEmptyList(t *testing.T) {
	t.Parallel()

	rec := postJSON(t, newAdminHandler(&stubAdmin{}).CreateMovie, "/v1/admin/movies", `{"title":"Dune","duration_min":155}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if body := decode[map[string]any](t, rec); !reflect.DeepEqual(body["genres"], []any{}) {
		t.Errorf("genres = %#v, want []", body["genres"])
	}
}

func TestCreateMovieErrors(t *testing.T) {
	t.Parallel()

	var v domain.Violations
	v.Add("title", "is required")
	rec := postJSON(t, newAdminHandler(&stubAdmin{err: v.Err()}).CreateMovie, "/v1/admin/movies", `{"duration_min":155}`)
	p := assertProblem(t, rec, http.StatusBadRequest, problem.CodeValidationFailed)
	if len(p.Errors) != 1 || p.Errors[0].Field != "title" {
		t.Errorf("errors = %+v", p.Errors)
	}

	rec = postJSON(t, newAdminHandler(&stubAdmin{}).CreateMovie, "/v1/admin/movies", `{"title":"Dune","duration_min":"long"}`)
	assertProblem(t, rec, http.StatusBadRequest, problem.CodeValidationFailed)
}

func TestCreateHall(t *testing.T) {
	t.Parallel()

	svc := &stubAdmin{}
	rec := postJSON(t, newAdminHandler(svc).CreateHall, "/v1/admin/halls",
		`{"name":"Hall 4","rows":[{"label":"A","seats":2},{"label":"B","seats":1,"type":"vip"}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q; halls have no URL of their own", loc)
	}
	wantRows := []domain.HallRow{{Label: "A", Seats: 2}, {Label: "B", Seats: 1, Type: domain.SeatVIP}}
	if svc.hall.Name != "Hall 4" || !slices.Equal(svc.hall.Rows, wantRows) {
		t.Errorf("service got %+v, want Hall 4 with %+v", svc.hall, wantRows)
	}

	body := decode[struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		SeatsTotal int    `json:"seats_total"`
		Seats      []struct {
			ID     int64  `json:"id"`
			Row    string `json:"row"`
			Number int    `json:"number"`
			Type   string `json:"type"`
		} `json:"seats"`
	}](t, rec)
	if body.ID != 4 || body.Name != "Hall 4" || body.SeatsTotal != 3 || len(body.Seats) != 3 {
		t.Fatalf("body = %+v", body)
	}
	if s := body.Seats[2]; s.ID != 303 || s.Row != "B" || s.Number != 1 || s.Type != "vip" {
		t.Errorf("last seat = %+v", s)
	}
}

func TestCreateHallErrors(t *testing.T) {
	t.Parallel()

	rec := postJSON(t, newAdminHandler(&stubAdmin{err: domain.Conflict(domain.CodeHallNameTaken, "taken")}).CreateHall,
		"/v1/admin/halls", `{"name":"Hall 1","rows":[{"label":"A","seats":2}]}`)
	assertProblem(t, rec, http.StatusConflict, domain.CodeHallNameTaken)

	svc := &stubAdmin{}
	rec = postJSON(t, newAdminHandler(svc).CreateHall, "/v1/admin/halls", `{"name":"Hall 4","rows":[{"label":"A","seats":"2"}]}`)
	p := assertProblem(t, rec, http.StatusBadRequest, problem.CodeValidationFailed)
	if len(p.Errors) != 1 || p.Errors[0].Field != "rows[0].seats" {
		t.Errorf("errors = %+v, want rows[0].seats, named like the service names row fields", p.Errors)
	}
	if svc.calls != 0 {
		t.Error("a malformed body reached the service")
	}
}

func TestCreateShowtime(t *testing.T) {
	t.Parallel()

	svc := &stubAdmin{}
	rec := postJSON(t, newAdminHandler(svc).CreateShowtime, "/v1/admin/showtimes",
		`{"movie_id":1,"hall_id":4,"starts_at":"2030-01-10T19:30:00+04:00","audio_language":"eng",
		  "subtitle_language":"tha","base_price_cents":1100}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/showtimes/77" {
		t.Errorf("Location = %q, want /v1/showtimes/77", loc)
	}
	start := time.Date(2030, 1, 10, 15, 30, 0, 0, time.UTC)
	if svc.showtime.MovieID != 1 || svc.showtime.HallID != 4 || !svc.showtime.StartsAt.Equal(start) ||
		svc.showtime.BasePriceCents != 1100 || svc.showtime.Language != (domain.LanguageVersion{Audio: "eng", Subtitles: "tha"}) {
		t.Errorf("service got %+v", svc.showtime)
	}

	body := decode[map[string]any](t, rec)
	for k, v := range map[string]any{
		"id": 77.0, "starts_at": "2030-01-10T19:30:00+04:00", "ends_at": "2030-01-10T22:20:00+04:00",
		"status": "scheduled", "base_price_cents": 1100.0, "currency": "EUR", "seats_available": 3.0, "seats_total": 3.0,
		"audio_language": "eng", "subtitle_language": "tha",
	} {
		if body[k] != v {
			t.Errorf("%s = %v, want %v", k, body[k], v)
		}
	}
}

func TestCreateShowtimeWithoutSubtitlesOmitsThem(t *testing.T) {
	t.Parallel()

	svc := &stubAdmin{}
	rec := postJSON(t, newAdminHandler(svc).CreateShowtime, "/v1/admin/showtimes",
		`{"movie_id":1,"hall_id":4,"starts_at":"2030-01-10T19:30:00Z","audio_language":"tha","base_price_cents":1100}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if svc.showtime.Language != (domain.LanguageVersion{Audio: "tha"}) {
		t.Errorf("service got %+v", svc.showtime.Language)
	}
	body := decode[map[string]any](t, rec)
	if _, ok := body["subtitle_language"]; ok || body["audio_language"] != "tha" {
		t.Errorf("body = %v, want audio_language tha and no subtitle_language", body)
	}
}

func TestCreateShowtimeErrors(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		body   string
		err    error
		status int
		code   string
		field  string
	}{
		{
			name: "overlap", body: `{"movie_id":1,"hall_id":4,"starts_at":"2030-01-10T19:30:00Z","base_price_cents":1}`,
			err: domain.Conflict(domain.CodeHallOverlap, "overlap"), status: http.StatusConflict, code: domain.CodeHallOverlap,
		},
		{
			name: "unknown hall", body: `{"movie_id":1,"hall_id":9,"starts_at":"2030-01-10T19:30:00Z","base_price_cents":1}`,
			err: domain.NotFound(domain.CodeHallNotFound, "no hall"), status: http.StatusNotFound, code: domain.CodeHallNotFound,
		},
		{
			name: "no time zone", body: `{"movie_id":1,"hall_id":4,"starts_at":"2030-01-10T19:30:00","base_price_cents":1}`,
			status: http.StatusBadRequest, code: problem.CodeValidationFailed, field: "starts_at",
		},
		{
			name: "date only", body: `{"movie_id":1,"hall_id":4,"starts_at":"2030-01-10","base_price_cents":1}`,
			status: http.StatusBadRequest, code: problem.CodeValidationFailed, field: "starts_at",
		},
		{
			name: "number", body: `{"movie_id":1,"hall_id":4,"starts_at":1893456000}`,
			status: http.StatusBadRequest, code: problem.CodeValidationFailed, field: "starts_at",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc := &stubAdmin{err: tt.err}
			rec := postJSON(t, newAdminHandler(svc).CreateShowtime, "/v1/admin/showtimes", tt.body)
			p := assertProblem(t, rec, tt.status, tt.code)
			if tt.field != "" {
				if len(p.Errors) != 1 || p.Errors[0].Field != tt.field {
					t.Errorf("errors = %+v, want %s", p.Errors, tt.field)
				}
				if svc.calls != 0 {
					t.Error("an unreadable start time reached the service")
				}
			}
		})
	}
}

func TestSetMovieGenres(t *testing.T) {
	t.Parallel()

	svc := &stubAdmin{}
	rec := serveAdmin(t, svc, http.MethodPut, "/v1/admin/movies/42/genres", `{"genre_ids":[7,3]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if svc.movieID != 42 || !slices.Equal(svc.genreIDs, []int64{7, 3}) {
		t.Errorf("service got movie %d with %v, want 42 with [7 3]", svc.movieID, svc.genreIDs)
	}
	body := decode[map[string]any](t, rec)
	if body["id"] != 42.0 || !reflect.DeepEqual(body["genres"], genresJSON(stubGenres([]int64{7, 3})...)) {
		t.Errorf("body = %v", body)
	}

	// [] clears the genres; the answer lists none.
	rec = serveAdmin(t, svc, http.MethodPut, "/v1/admin/movies/42/genres", `{"genre_ids":[]}`)
	if rec.Code != http.StatusOK || svc.genreIDs == nil || len(svc.genreIDs) != 0 {
		t.Errorf("clearing: %d, service got %#v; want 200 and an empty list", rec.Code, svc.genreIDs)
	}
	if body := decode[map[string]any](t, rec); !reflect.DeepEqual(body["genres"], []any{}) {
		t.Errorf("genres = %#v, want []", body["genres"])
	}
}

func TestSetMovieGenresErrors(t *testing.T) {
	t.Parallel()

	svc := &stubAdmin{}
	for _, body := range []string{`{}`, `{"genre_ids":null}`} {
		p := assertProblem(t, serveAdmin(t, svc, http.MethodPut, "/v1/admin/movies/42/genres", body),
			http.StatusBadRequest, problem.CodeValidationFailed)
		if len(p.Errors) != 1 || p.Errors[0].Field != "genre_ids" {
			t.Errorf("%s: errors = %+v, want genre_ids", body, p.Errors)
		}
	}
	assertProblem(t, serveAdmin(t, svc, http.MethodPut, "/v1/admin/movies/x/genres", `{"genre_ids":[]}`),
		http.StatusBadRequest, problem.CodeValidationFailed)
	assertProblem(t, serveAdmin(t, svc, http.MethodPut, "/v1/admin/movies/42/genres", `{"genre_ids":["drama"]}`),
		http.StatusBadRequest, problem.CodeValidationFailed)
	if svc.calls != 0 {
		t.Errorf("%d bad requests reached the service", svc.calls)
	}

	notFound := &stubAdmin{err: domain.NotFound(domain.CodeMovieNotFound, "movie 42 not found")}
	assertProblem(t, serveAdmin(t, notFound, http.MethodPut, "/v1/admin/movies/42/genres", `{"genre_ids":[1]}`),
		http.StatusNotFound, domain.CodeMovieNotFound)
	var v domain.Violations
	v.Add("genre_ids[0]", "no genre has the id 99")
	p := assertProblem(t, serveAdmin(t, &stubAdmin{err: v.Err()}, http.MethodPut, "/v1/admin/movies/42/genres", `{"genre_ids":[99]}`),
		http.StatusBadRequest, problem.CodeValidationFailed)
	if len(p.Errors) != 1 || p.Errors[0].Field != "genre_ids[0]" {
		t.Errorf("errors = %+v", p.Errors)
	}
}

func TestGenreCRUD(t *testing.T) {
	t.Parallel()

	svc := &stubAdmin{}
	rec := serveAdmin(t, svc, http.MethodPost, "/v1/admin/genres", `{"slug":"film_noir","name":"Film noir"}`)
	if rec.Code != http.StatusCreated || rec.Header().Get("Location") != "/v1/genres/19" {
		t.Fatalf("create: %d, Location %q, body %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if svc.genre != (domain.NewGenre{Slug: "film_noir", Name: "Film noir"}) {
		t.Errorf("service got %+v", svc.genre)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"id":19,"slug":"film_noir","name":"Film noir"}` {
		t.Errorf("create body = %s", got)
	}

	rec = serveAdmin(t, svc, http.MethodPut, "/v1/admin/genres/19", `{"slug":"noir","name":"Noir"}`)
	if rec.Code != http.StatusOK || svc.genreID != 19 || svc.genre != (domain.NewGenre{Slug: "noir", Name: "Noir"}) {
		t.Errorf("update: %d, service got %d %+v", rec.Code, svc.genreID, svc.genre)
	}

	rec = serveAdmin(t, svc, http.MethodDelete, "/v1/admin/genres/19", "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 || svc.genreID != 19 {
		t.Errorf("delete: %d with %q, service got %d", rec.Code, rec.Body.String(), svc.genreID)
	}
}

func TestGenreCRUDErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, method, target, body string
		err                        error
		status                     int
		code                       string
	}{
		{"unknown field", http.MethodPost, "/v1/admin/genres", `{"slug":"noir","title":"Noir"}`, nil, http.StatusBadRequest, problem.CodeMalformedBody},
		{"slug taken", http.MethodPost, "/v1/admin/genres", `{"slug":"drama","name":"Drama 2"}`,
			domain.Conflict(domain.CodeGenreSlugTaken, "taken"), http.StatusConflict, domain.CodeGenreSlugTaken},
		{"name taken", http.MethodPut, "/v1/admin/genres/3", `{"slug":"dramas","name":"drama"}`,
			domain.Conflict(domain.CodeGenreNameTaken, "taken"), http.StatusConflict, domain.CodeGenreNameTaken},
		{"update unknown", http.MethodPut, "/v1/admin/genres/99", `{"slug":"x","name":"X"}`,
			domain.NotFound(domain.CodeGenreNotFound, "no"), http.StatusNotFound, domain.CodeGenreNotFound},
		{"update bad id", http.MethodPut, "/v1/admin/genres/0", `{"slug":"x","name":"X"}`, nil, http.StatusBadRequest, problem.CodeValidationFailed},
		{"delete in use", http.MethodDelete, "/v1/admin/genres/3", "",
			domain.Conflict(domain.CodeGenreInUse, "in use"), http.StatusConflict, domain.CodeGenreInUse},
		{"delete unknown", http.MethodDelete, "/v1/admin/genres/99", "",
			domain.NotFound(domain.CodeGenreNotFound, "no"), http.StatusNotFound, domain.CodeGenreNotFound},
		{"delete bad id", http.MethodDelete, "/v1/admin/genres/drama", "", nil, http.StatusBadRequest, problem.CodeValidationFailed},
		{"server failure", http.MethodDelete, "/v1/admin/genres/3", "", errors.New("db down"), http.StatusInternalServerError, problem.CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertProblem(t, serveAdmin(t, &stubAdmin{err: tt.err}, tt.method, tt.target, tt.body), tt.status, tt.code)
		})
	}
}
