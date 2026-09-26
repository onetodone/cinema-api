package dto

import "github.com/onetodone/cinema-api/internal/domain"

// CreateMovieRequest is the body of POST /v1/admin/movies.
type CreateMovieRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	DurationMin int    `json:"duration_min"`
	AgeRating   string `json:"age_rating"`
	PosterURL   string `json:"poster_url"`
}

// NewMovie maps the request to the domain input.
func (r CreateMovieRequest) NewMovie() domain.NewMovie {
	return domain.NewMovie{
		Title:       r.Title,
		Description: r.Description,
		DurationMin: r.DurationMin,
		AgeRating:   r.AgeRating,
		PosterURL:   r.PosterURL,
	}
}
