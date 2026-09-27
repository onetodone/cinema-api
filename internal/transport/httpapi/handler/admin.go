package handler

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/dto"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/render"
)

// AdminService is the part of the catalog management use cases that the HTTP layer needs.
type AdminService interface {
	CreateMovie(ctx context.Context, m domain.NewMovie) (domain.Movie, error)
	CreateHall(ctx context.Context, h domain.NewHall) (domain.HallLayout, error)
	CreateShowtime(ctx context.Context, s domain.NewShowtime) (domain.Showtime, error)
	SetMovieGenres(ctx context.Context, movieID int64, genreIDs []int64) (domain.Movie, error)
	CreateGenre(ctx context.Context, g domain.NewGenre) (domain.Genre, error)
	UpdateGenre(ctx context.Context, id int64, g domain.NewGenre) (domain.Genre, error)
	DeleteGenre(ctx context.Context, id int64) error
}

// Admin serves the catalog management endpoints. The router lets only admins reach them.
type Admin struct {
	svc      AdminService
	currency string
	logger   *slog.Logger
}

// NewAdmin returns the admin handlers. currency is the ISO 4217 code reported next to prices.
func NewAdmin(svc AdminService, currency string, logger *slog.Logger) *Admin {
	return &Admin{svc: svc, currency: currency, logger: logger}
}

// CreateMovie handles POST /v1/admin/movies. It answers 201 with the movie and its URL in Location.
func (h *Admin) CreateMovie(w http.ResponseWriter, r *http.Request) {
	var req dto.CreateMovieRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	m, err := h.svc.CreateMovie(r.Context(), req.NewMovie())
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/movies/"+strconv.FormatInt(m.ID, 10))
	render.JSON(w, http.StatusCreated, dto.NewMovie(m))
}

// SetMovieGenres handles PUT /v1/admin/movies/{movieID}/genres. It answers 200 with the movie and its new genres.
// genre_ids is required: an empty list clears the genres, a missing one is refused.
func (h *Admin) SetMovieGenres(w http.ResponseWriter, r *http.Request) {
	var p params
	movieID := p.pathID(r, "movieID")
	if !p.ok(w, r) {
		return
	}
	var req dto.SetMovieGenresRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.GenreIDs == nil {
		p.fail("genre_ids", "is required; send [] to remove every genre")
	}
	if !p.ok(w, r) {
		return
	}

	m, err := h.svc.SetMovieGenres(r.Context(), movieID, req.GenreIDs)
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	render.JSON(w, http.StatusOK, dto.NewMovie(m))
}

// CreateGenre handles POST /v1/admin/genres. It answers 201 with the genre and its URL in Location.
func (h *Admin) CreateGenre(w http.ResponseWriter, r *http.Request) {
	var req dto.GenreRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	g, err := h.svc.CreateGenre(r.Context(), req.NewGenre())
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/genres/"+strconv.FormatInt(g.ID, 10))
	render.JSON(w, http.StatusCreated, dto.NewGenre(g))
}

// UpdateGenre handles PUT /v1/admin/genres/{genreID}. It answers 200 with the genre.
func (h *Admin) UpdateGenre(w http.ResponseWriter, r *http.Request) {
	var p params
	id := p.pathID(r, "genreID")
	if !p.ok(w, r) {
		return
	}
	var req dto.GenreRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	g, err := h.svc.UpdateGenre(r.Context(), id, req.NewGenre())
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	render.JSON(w, http.StatusOK, dto.NewGenre(g))
}

// DeleteGenre handles DELETE /v1/admin/genres/{genreID}. It answers 204.
func (h *Admin) DeleteGenre(w http.ResponseWriter, r *http.Request) {
	var p params
	id := p.pathID(r, "genreID")
	if !p.ok(w, r) {
		return
	}

	if err := h.svc.DeleteGenre(r.Context(), id); err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CreateHall handles POST /v1/admin/halls. It answers 201 with the hall and its generated seats. Halls have no URL
// of their own; showtimes and seat maps show them.
func (h *Admin) CreateHall(w http.ResponseWriter, r *http.Request) {
	var req dto.CreateHallRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	hall, err := h.svc.CreateHall(r.Context(), req.NewHall())
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	render.JSON(w, http.StatusCreated, dto.NewHall(hall))
}

// CreateShowtime handles POST /v1/admin/showtimes. It answers 201 with the showtime and its URL in Location; every
// seat of the hall is on sale from then on.
func (h *Admin) CreateShowtime(w http.ResponseWriter, r *http.Request) {
	var req dto.CreateShowtimeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	var p params
	startsAt := p.timestamp("starts_at", req.StartsAt)
	if !p.ok(w, r) {
		return
	}

	st, err := h.svc.CreateShowtime(r.Context(), req.NewShowtime(startsAt))
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/showtimes/"+strconv.FormatInt(st.ID, 10))
	render.JSON(w, http.StatusCreated, dto.NewShowtime(st, h.currency))
}
