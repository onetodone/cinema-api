package handler

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

// stubAdmin records the movie it receives.
type stubAdmin struct {
	got domain.NewMovie
	err error
}

func (s *stubAdmin) CreateMovie(_ context.Context, m domain.NewMovie) (domain.Movie, error) {
	s.got = m
	if s.err != nil {
		return domain.Movie{}, s.err
	}
	return domain.Movie{ID: 42, Title: m.Title, DurationMin: m.DurationMin, AgeRating: m.AgeRating}, nil
}

func TestCreateMovie(t *testing.T) {
	t.Parallel()

	svc := &stubAdmin{}
	h := NewAdmin(svc, slog.New(slog.DiscardHandler))
	rec := postJSON(t, h.CreateMovie, "/v1/admin/movies",
		`{"title":"Dune","description":"Sand.","duration_min":155,"age_rating":"PG-13","poster_url":"https://x/y.jpg"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/movies/42" {
		t.Errorf("Location = %q, want /v1/movies/42", loc)
	}
	want := domain.NewMovie{
		Title: "Dune", Description: "Sand.", DurationMin: 155, AgeRating: "PG-13", PosterURL: "https://x/y.jpg",
	}
	if svc.got != want {
		t.Errorf("service got %+v, want %+v", svc.got, want)
	}
	if body := decode[map[string]any](t, rec); body["id"] != 42.0 || body["title"] != "Dune" {
		t.Errorf("body = %v", body)
	}
}

func TestCreateMovieErrors(t *testing.T) {
	t.Parallel()

	var v domain.Violations
	v.Add("title", "is required")
	rec := postJSON(t, NewAdmin(&stubAdmin{err: v.Err()}, slog.New(slog.DiscardHandler)).CreateMovie,
		"/v1/admin/movies", `{"duration_min":155}`)
	p := assertProblem(t, rec, http.StatusBadRequest, problem.CodeValidationFailed)
	if len(p.Errors) != 1 || p.Errors[0].Field != "title" {
		t.Errorf("errors = %+v", p.Errors)
	}

	rec = postJSON(t, NewAdmin(&stubAdmin{}, slog.New(slog.DiscardHandler)).CreateMovie,
		"/v1/admin/movies", `{"title":"Dune","duration_min":"long"}`)
	assertProblem(t, rec, http.StatusBadRequest, problem.CodeValidationFailed)
}
