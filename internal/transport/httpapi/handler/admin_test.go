package handler

import (
	"context"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
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
	calls    int
	err      error
}

func (s *stubAdmin) CreateMovie(_ context.Context, m domain.NewMovie) (domain.Movie, error) {
	s.calls++
	s.movie = m
	if s.err != nil {
		return domain.Movie{}, s.err
	}
	return domain.Movie{ID: 42, Title: m.Title, DurationMin: m.DurationMin, AgeRating: m.AgeRating, Genres: m.Genres}, nil
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

func TestCreateMovie(t *testing.T) {
	t.Parallel()

	svc := &stubAdmin{}
	rec := postJSON(t, newAdminHandler(svc).CreateMovie, "/v1/admin/movies",
		`{"title":"Dune","description":"Sand.","duration_min":155,"age_rating":"PG-13","poster_url":"https://x/y.jpg",
		  "genres":["science_fiction","adventure"]}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/movies/42" {
		t.Errorf("Location = %q, want /v1/movies/42", loc)
	}
	want := domain.NewMovie{
		Title: "Dune", Description: "Sand.", DurationMin: 155, AgeRating: "PG-13", PosterURL: "https://x/y.jpg",
		Genres: []domain.Genre{domain.GenreScienceFiction, domain.GenreAdventure},
	}
	if !reflect.DeepEqual(svc.movie, want) {
		t.Errorf("service got %+v, want %+v", svc.movie, want)
	}
	body := decode[map[string]any](t, rec)
	if body["id"] != 42.0 || body["title"] != "Dune" || !reflect.DeepEqual(body["genres"], []any{"science_fiction", "adventure"}) {
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
