package handler

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/service/catalog"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/dto"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/render"
)

// CatalogService is the part of the catalog use cases that the HTTP layer needs.
type CatalogService interface {
	ListMovies(ctx context.Context, afterID int64, limit int) (catalog.MoviePage, error)
	GetMovie(ctx context.Context, id int64) (catalog.MovieDetails, error)
	Schedule(ctx context.Context, q catalog.ScheduleQuery) (catalog.Schedule, error)
	GetShowtime(ctx context.Context, id int64) (domain.Showtime, error)
	SeatMap(ctx context.Context, showtimeID int64) (catalog.SeatMap, error)
}

// Catalog serves the public browsing endpoints.
type Catalog struct {
	svc      CatalogService
	currency string
	logger   *slog.Logger
}

// NewCatalog returns the catalog handlers. currency is the ISO 4217 code reported next to prices.
func NewCatalog(svc CatalogService, currency string, logger *slog.Logger) *Catalog {
	return &Catalog{svc: svc, currency: currency, logger: logger}
}

// ListMovies handles GET /v1/movies?limit=&cursor=.
func (h *Catalog) ListMovies(w http.ResponseWriter, r *http.Request) {
	var p params
	limit, afterID := p.limit(r, catalog.MaxPageSize), p.cursor(r)
	if !p.ok(w, r) {
		return
	}

	page, err := h.svc.ListMovies(r.Context(), afterID, limit)
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	render.JSON(w, http.StatusOK, dto.NewMovieList(page, encodeCursor))
}

// GetMovie handles GET /v1/movies/{movieID}.
func (h *Catalog) GetMovie(w http.ResponseWriter, r *http.Request) {
	var p params
	id := p.pathID(r, "movieID")
	if !p.ok(w, r) {
		return
	}

	details, err := h.svc.GetMovie(r.Context(), id)
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	render.JSON(w, http.StatusOK, dto.NewMovieDetails(details, h.currency))
}

// Schedule handles GET /v1/showtimes?date=YYYY-MM-DD&movie_id=. Without a date it lists today. Clients poll it
// with If-None-Match (see render.ValidatedJSON).
func (h *Catalog) Schedule(w http.ResponseWriter, r *http.Request) {
	var p params
	q := catalog.ScheduleQuery{Day: p.date(r, "date"), MovieID: p.optionalID(r, "movie_id")}
	if !p.ok(w, r) {
		return
	}

	sched, err := h.svc.Schedule(r.Context(), q)
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	render.ValidatedJSON(w, r, dto.NewSchedule(sched, h.currency))
}

// GetShowtime handles GET /v1/showtimes/{showtimeID}.
func (h *Catalog) GetShowtime(w http.ResponseWriter, r *http.Request) {
	var p params
	id := p.pathID(r, "showtimeID")
	if !p.ok(w, r) {
		return
	}

	st, err := h.svc.GetShowtime(r.Context(), id)
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	render.JSON(w, http.StatusOK, dto.NewShowtime(st, h.currency))
}

// SeatMap handles GET /v1/showtimes/{showtimeID}/seats. Clients poll it with If-None-Match (see
// render.ValidatedJSON).
func (h *Catalog) SeatMap(w http.ResponseWriter, r *http.Request) {
	var p params
	id := p.pathID(r, "showtimeID")
	if !p.ok(w, r) {
		return
	}

	sm, err := h.svc.SeatMap(r.Context(), id)
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	render.ValidatedJSON(w, r, dto.NewSeatMap(sm, h.currency))
}
