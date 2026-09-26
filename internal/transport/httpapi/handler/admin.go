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
}

// Admin serves the catalog management endpoints. The router lets only admins reach them.
type Admin struct {
	svc    AdminService
	logger *slog.Logger
}

// NewAdmin returns the admin handlers.
func NewAdmin(svc AdminService, logger *slog.Logger) *Admin {
	return &Admin{svc: svc, logger: logger}
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
