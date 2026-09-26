// Package admin implements the catalog management use cases that only administrators may run.
package admin

import (
	"context"
	"strings"

	"github.com/onetodone/cinema-api/internal/domain"
)

// Repository is the catalog storage the admin use cases write to. It is implemented by
// repository/postgres.Catalog.
type Repository interface {
	CreateMovie(ctx context.Context, m domain.NewMovie) (domain.Movie, error)
}

// Service manages the catalog. Callers must check that the caller is an admin; the HTTP layer does that.
type Service struct {
	repo Repository
}

// New returns an admin Service.
func New(repo Repository) *Service {
	return &Service{repo: repo}
}

// CreateMovie adds a movie. Surrounding space is trimmed from every text field. Invalid input fails with a
// *domain.ValidationError listing every bad field.
func (s *Service) CreateMovie(ctx context.Context, m domain.NewMovie) (domain.Movie, error) {
	m.Title = strings.TrimSpace(m.Title)
	m.Description = strings.TrimSpace(m.Description)
	m.AgeRating = strings.TrimSpace(m.AgeRating)
	m.PosterURL = strings.TrimSpace(m.PosterURL)
	if err := m.Validate(); err != nil {
		return domain.Movie{}, err
	}
	return s.repo.CreateMovie(ctx, m)
}
