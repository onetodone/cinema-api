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
