package domain

import (
	"slices"
	"strings"
	"time"
	"uuid"
)

// PaymentStatus is the state of one payment attempt for a booking.
type PaymentStatus string

// Payment statuses. Only a pending payment can change; the others are final, except that a failed payment can
// still be refunded (see paymentTransitions).
const (
	PaymentPending   PaymentStatus = "pending"   // the charge is in flight, or its outcome is not known yet
	PaymentSucceeded PaymentStatus = "succeeded" // charged; the booking is paid
	PaymentFailed    PaymentStatus = "failed"    // nothing was charged
	PaymentRefunded  PaymentStatus = "refunded"  // charged, and the money was returned
)

// paymentTransitions lists the allowed status changes. A payment that the worker settled as failed, because its
// provider had no charge for it yet, may still be charged when the API's call to the provider answers late; the
// API then refunds the charge, and the payment moves from failed to refunded.
var paymentTransitions = map[PaymentStatus][]PaymentStatus{
	PaymentPending: {PaymentSucceeded, PaymentFailed, PaymentRefunded},
	PaymentFailed:  {PaymentRefunded},
}

// CanBecome reports whether a payment in status s may change to next.
func (s PaymentStatus) CanBecome(next PaymentStatus) bool {
	return slices.Contains(paymentTransitions[s], next)
}

// Payment is one attempt to pay for a booking. A booking may have several failed attempts, but at most one
// pending and at most one succeeded payment.
type Payment struct {
	ID            uuid.UUID // UUIDv7; also the idempotency key at the provider
	BookingID     uuid.UUID
	Provider      string // id of the payment provider, such as "local"
	AmountCents   int64
	Status        PaymentStatus
	ProviderRef   string // the provider's id of the charge, once known
	FailureReason string // why the payment failed or was refunded, such as "insufficient_funds"
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// PaymentOutcome is how a pending payment ends.
type PaymentOutcome struct {
	Status        PaymentStatus // succeeded, failed, or refunded
	ProviderRef   string
	FailureReason string
}

// maxPaymentTokenBytes caps payment tokens. Provider tokens are short identifiers; anything longer is not one.
const maxPaymentTokenBytes = 255

// NewPayment is a request to pay for a booking.
type NewPayment struct {
	Method string // id of the payment provider, as GET /v1/payment-methods lists it
	Token  string // stands for the payment source at that provider; opaque to this service
}

// Validate reports every invalid field of p as a *ValidationError, or returns nil.
func (p NewPayment) Validate() error {
	var v Violations
	if strings.TrimSpace(p.Method) == "" {
		v.Add("payment_method", "is required")
	}
	switch {
	case strings.TrimSpace(p.Token) == "":
		v.Add("payment_token", "is required")
	case len(p.Token) > maxPaymentTokenBytes:
		v.Add("payment_token", "must be at most %d bytes", maxPaymentTokenBytes)
	}
	return v.Err()
}

// CheckPayable reports why a booking cannot be paid, or returns nil. holdOver tells whether the booking's hold
// has run out, judged by the database clock: a pending booking past its deadline is expired even if the expiry
// worker has not been there yet.
func CheckPayable(b Booking, holdOver bool) error {
	switch b.Status {
	case BookingPending:
		if holdOver {
			return ExpiredBooking(b.ID)
		}
		return nil
	case BookingProcessing:
		return Conflict(CodePaymentInProgress, "a payment for booking %s is already in progress", b.ID)
	case BookingPaid:
		return Conflict(CodeBookingAlreadyPaid, "booking %s is already paid", b.ID)
	case BookingExpired:
		return ExpiredBooking(b.ID)
	default:
		return Conflict(CodeBookingCanceled, "booking %s is canceled", b.ID)
	}
}

// ExpiredBooking returns the BOOKING_EXPIRED error: the hold ran out before the booking was paid.
func ExpiredBooking(id uuid.UUID) error {
	return Gone(CodeBookingExpired, "the hold of booking %s has run out; book the seats again", id)
}

// PaymentDeclinedError reports a charge that the payment provider declined. It is of kind ErrPaymentRequired
// with code CodePaymentDeclined: errors.Is(err, ErrPaymentRequired) holds, and errors.As finds the *Error.
type PaymentDeclinedError struct {
	DeclineCode string // the provider's reason, such as "insufficient_funds"
	err         *Error
}

// PaymentDeclined returns a *PaymentDeclinedError with the provider's decline code.
func PaymentDeclined(declineCode string) error {
	return &PaymentDeclinedError{
		DeclineCode: declineCode,
		err: &Error{
			Kind: ErrPaymentRequired, Code: CodePaymentDeclined,
			Message: "the payment was declined (" + declineCode + "); nothing was charged",
		},
	}
}

func (e *PaymentDeclinedError) Error() string { return e.err.Message }

// Unwrap exposes the underlying *Error, so the code and kind are visible to errors.As and errors.Is.
func (e *PaymentDeclinedError) Unwrap() error { return e.err }
