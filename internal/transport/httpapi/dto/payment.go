package dto

import (
	"time"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/payment"
	"github.com/onetodone/cinema-api/internal/service/booking"
)

// PayRequest is the body of POST /v1/bookings/{bookingID}/payments.
type PayRequest struct {
	PaymentMethod string `json:"payment_method"` // a provider id from GET /v1/payment-methods
	PaymentToken  string `json:"payment_token"`
}

// NewPayment maps the request to the domain input.
func (r PayRequest) NewPayment() domain.NewPayment {
	return domain.NewPayment{Method: r.PaymentMethod, Token: r.PaymentToken}
}

// Payment is one payment attempt as the booking's owner sees it.
type Payment struct {
	ID            string    `json:"id"`
	Status        string    `json:"status"`
	PaymentMethod string    `json:"payment_method"`
	AmountCents   int64     `json:"amount_cents"`
	Currency      string    `json:"currency"`
	FailureReason string    `json:"failure_reason,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// PaymentResult is the answer to a payment: the payment and its booking afterwards.
type PaymentResult struct {
	Payment Payment `json:"payment"`
	Booking Booking `json:"booking"`
}

// NewPaymentResult maps the outcome of booking.Service.Pay.
func NewPaymentResult(res booking.PayResult, currency string) PaymentResult {
	p := res.Payment
	return PaymentResult{
		Payment: Payment{
			ID:            p.ID.String(),
			Status:        string(p.Status),
			PaymentMethod: p.Provider,
			AmountCents:   p.AmountCents,
			Currency:      currency,
			FailureReason: p.FailureReason,
			CreatedAt:     p.CreatedAt.UTC(),
		},
		Booking: NewBooking(res.Booking, currency),
	}
}

// PaymentMethod is a way to pay that clients can choose.
type PaymentMethod struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// PaymentMethodList lists the payment methods that take payments now.
type PaymentMethodList struct {
	Items []PaymentMethod `json:"items"`
}

// NewPaymentMethodList maps the enabled payment providers.
func NewPaymentMethodList(methods []payment.Method) PaymentMethodList {
	out := PaymentMethodList{Items: make([]PaymentMethod, 0, len(methods))}
	for _, m := range methods {
		out.Items = append(out.Items, PaymentMethod{ID: m.ID, Name: m.Name})
	}
	return out
}
