package admin

import (
	"context"
	"errors"
	"testing"

	"github.com/onetodone/cinema-api/internal/domain"
)

// recordingRepo remembers the movie it was asked to create.
type recordingRepo struct {
	got   *domain.NewMovie
	err   error
	calls int
}

func (r *recordingRepo) CreateMovie(_ context.Context, m domain.NewMovie) (domain.Movie, error) {
	r.calls++
	r.got = &m
	if r.err != nil {
		return domain.Movie{}, r.err
	}
	return domain.Movie{ID: 7, Title: m.Title, DurationMin: m.DurationMin}, nil
}

func TestCreateMovieTrimsAndStores(t *testing.T) {
	t.Parallel()

	repo := &recordingRepo{}
	m, err := New(repo).CreateMovie(t.Context(), domain.NewMovie{
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
	if *repo.got != want {
		t.Errorf("stored %+v, want %+v", *repo.got, want)
	}
}

func TestCreateMovieRejectsInvalidInputBeforeStoring(t *testing.T) {
	t.Parallel()

	repo := &recordingRepo{}
	_, err := New(repo).CreateMovie(t.Context(), domain.NewMovie{Title: "   ", DurationMin: 0})

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
	if _, err := New(repo).CreateMovie(t.Context(), domain.NewMovie{Title: "Dune", DurationMin: 155}); err == nil {
		t.Error("the repository error was swallowed")
	}
}
