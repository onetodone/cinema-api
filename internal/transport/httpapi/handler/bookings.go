package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/service/booking"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/dto"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/render"
)

// BookingService is the part of the booking use cases that the HTTP layer needs.
type BookingService interface {
	Create(ctx context.Context, userID uuid.UUID, nb domain.NewBooking) (domain.Booking, error)
	Get(ctx context.Context, userID, id uuid.UUID) (domain.Booking, error)
	List(ctx context.Context, userID, beforeID uuid.UUID, limit int) (booking.Page, error)
	Cancel(ctx context.Context, userID, id uuid.UUID) error
}

// Bookings serves the caller's own bookings. Every route must run behind the Authenticate middleware.
type Bookings struct {
	svc      BookingService
	currency string
	metrics  *metrics.Metrics
	logger   *slog.Logger
}

// NewBookings returns the booking handlers. currency is the ISO 4217 code reported next to prices; every booking
// attempt counts in m.
func NewBookings(svc BookingService, currency string, m *metrics.Metrics, logger *slog.Logger) *Bookings {
	return &Bookings{svc: svc, currency: currency, metrics: m, logger: logger}
}

// Create handles POST /v1/bookings. It answers 201 with the booking and its URL in Location.
func (h *Bookings) Create(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(h.logger, w, r)
	if !ok {
		return
	}
	var req dto.CreateBookingRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	b, err := h.svc.Create(r.Context(), p.UserID, req.NewBooking())
	h.metrics.BookingAttempts.WithLabelValues(bookingResult(err)).Inc()
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/bookings/"+b.ID.String())
	render.JSON(w, http.StatusCreated, dto.NewBooking(b, h.currency))
}

// bookingResult classifies the outcome of a booking attempt for cinema_booking_attempts_total.
func bookingResult(err error) string {
	var de *domain.Error
	switch {
	case err == nil:
		return metrics.BookingCreated
	case errors.Is(err, domain.ErrBusy):
		return metrics.BookingBusy
	case errors.As(err, &de) && de.Code == domain.CodeSeatUnavailable:
		return metrics.BookingSeatUnavailable
	case errors.As(err, &de) && de.Code == domain.CodeActiveBookingExists:
		return metrics.BookingActiveBookingExists
	case problem.FromError(err).Status < http.StatusInternalServerError:
		return metrics.BookingRejected
	default:
		return metrics.BookingFailed
	}
}

// List handles GET /v1/bookings?limit=&cursor=.
func (h *Bookings) List(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(h.logger, w, r)
	if !ok {
		return
	}
	var ps params
	limit, beforeID := ps.limit(r, booking.MaxPageSize), ps.uuidCursor(r)
	if !ps.ok(w, r) {
		return
	}

	page, err := h.svc.List(r.Context(), p.UserID, beforeID, limit)
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	render.JSON(w, http.StatusOK, dto.NewBookingList(page, h.currency, encodeUUIDCursor))
}

// Get handles GET /v1/bookings/{bookingID}. Bookings of other users are reported as not found.
func (h *Bookings) Get(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(h.logger, w, r)
	if !ok {
		return
	}
	var ps params
	id := ps.pathUUID(r, "bookingID")
	if !ps.ok(w, r) {
		return
	}

	b, err := h.svc.Get(r.Context(), p.UserID, id)
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	render.JSON(w, http.StatusOK, dto.NewBooking(b, h.currency))
}

// Cancel handles DELETE /v1/bookings/{bookingID}. It answers 204 once the booking holds no seats, also when
// it was already canceled or expired.
func (h *Bookings) Cancel(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(h.logger, w, r)
	if !ok {
		return
	}
	var ps params
	id := ps.pathUUID(r, "bookingID")
	if !ps.ok(w, r) {
		return
	}

	if err := h.svc.Cancel(r.Context(), p.UserID, id); err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
