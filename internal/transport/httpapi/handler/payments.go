package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/payment"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
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
	metrics  *metrics.Metrics
	logger   *slog.Logger
}

// NewPayments returns the payment handlers. currency is the ISO 4217 code reported next to amounts; every payment
// counts in m.
func NewPayments(svc PaymentService, methods PaymentMethods, currency string, m *metrics.Metrics, logger *slog.Logger) *Payments {
	return &Payments{svc: svc, methods: methods, currency: currency, metrics: m, logger: logger}
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
	if result, ok := paymentResult(res, err); ok {
		h.metrics.Payments.WithLabelValues(req.PaymentMethod, result).Inc()
	}
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

// paymentResult classifies the outcome of a payment for cinema_payments_total. ok is false when the request was
// turned away before a payment started, such as for an expired booking or an unknown payment method: only
// payments that reached an enabled provider count, which also keeps the provider label to known values.
func paymentResult(res booking.PayResult, err error) (result string, ok bool) {
	var de *domain.Error
	switch {
	case err == nil && res.Payment.Status == domain.PaymentPending:
		return metrics.PaymentPending, true
	case err == nil:
		return metrics.PaymentSucceeded, true
	case !errors.As(err, &de):
		return "", false
	}
	switch de.Code {
	case domain.CodePaymentDeclined:
		return metrics.PaymentDeclined, true
	case domain.CodePaymentProviderUnavailable:
		return metrics.PaymentProviderUnavailable, true
	case domain.CodePaymentRefunded:
		return metrics.PaymentRefunded, true
	default:
		return "", false
	}
}
