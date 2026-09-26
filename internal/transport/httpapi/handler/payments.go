package handler

import (
	"context"
	"log/slog"
	"net/http"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/payment"
	"github.com/onetodone/cinema-api/internal/service/booking"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/dto"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/render"
)

// PaymentService is the part of the booking use cases that paying needs.
type PaymentService interface {
	Pay(ctx context.Context, userID, bookingID uuid.UUID, np domain.NewPayment) (booking.PayResult, error)
}

// PaymentMethods lists the payment providers that take new payments. It is implemented by *payment.Registry.
type PaymentMethods interface {
	Methods() []payment.Method
}

// Payments serves the payment methods and payments for the caller's bookings.
type Payments struct {
	svc      PaymentService
	methods  PaymentMethods
	currency string
	logger   *slog.Logger
}

// NewPayments returns the payment handlers. currency is the ISO 4217 code reported next to amounts.
func NewPayments(svc PaymentService, methods PaymentMethods, currency string, logger *slog.Logger) *Payments {
	return &Payments{svc: svc, methods: methods, currency: currency, logger: logger}
}

// ListMethods handles GET /v1/payment-methods: the payment providers that take payments now.
func (h *Payments) ListMethods(w http.ResponseWriter, _ *http.Request) {
	render.JSON(w, http.StatusOK, dto.NewPaymentMethodList(h.methods.Methods()))
}

// Pay handles POST /v1/bookings/{bookingID}/payments. It must run behind the Authenticate middleware.
//
// It answers 200 with the paid booking. When the provider has not settled the charge yet, it answers 202 with
// the pending payment and the processing booking, and Location names the booking to follow: the worker settles
// the payment within the payment grace period.
func (h *Payments) Pay(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(h.logger, w, r)
	if !ok {
		return
	}
	var ps params
	bookingID := ps.pathUUID(r, "bookingID")
	if !ps.ok(w, r) {
		return
	}
	var req dto.PayRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	res, err := h.svc.Pay(r.Context(), p.UserID, bookingID, req.NewPayment())
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	status := http.StatusOK
	if res.Payment.Status == domain.PaymentPending {
		w.Header().Set("Location", "/v1/bookings/"+bookingID.String())
		status = http.StatusAccepted
	}
	render.JSON(w, status, dto.NewPaymentResult(res, h.currency))
}
