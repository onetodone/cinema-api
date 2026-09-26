// Package payment is the contract between the booking flow and payment providers, in the way that
// database/sql/driver is the contract between database/sql and database drivers. The booking service charges,
// refunds, and checks payments only through the Provider interface, and finds providers in a Registry.
//
// Adding a provider, such as Stripe or PayPal, takes an adapter package that implements Provider and passes the
// contract tests in paymenttest (payment/local is the reference implementation), plus one Register call in the
// composition root. Neither the booking service nor this package changes.
package payment

import (
	"context"
	"errors"
	"regexp"
	"uuid"
)

// Provider is a payment provider adapter. Implementations must be safe for concurrent use.
//
// The payment id is the idempotency key of every call. A provider charges a payment id at most once, however
// often Pay is called with it, and answers later calls with the result of the first one.
type Provider interface {
	// ID is the stable code that clients choose the provider by and that payments record, such as "local".
	// It must match ValidID.
	ID() string
	// Name is the provider's display name for clients, such as "Test card".
	Name() string
	// Pay charges the payment source that req.Token stands for.
	//
	// A nil error means a definite answer: the charge succeeded or was declined. An error means the charge is
	// not settled:
	//   - an error that wraps ErrUnavailable means the provider did not take the request, and nothing was
	//     charged;
	//   - any other error, such as a timeout or a dropped connection, means the outcome is unknown. The charge
	//     may have happened; Status tells later.
	Pay(ctx context.Context, req PayRequest) (Charge, error)
	// Refund returns the full amount of a succeeded charge. Refunding a charge that is already refunded
	// succeeds.
	Refund(ctx context.Context, req RefundRequest) error
	// Status reports what the provider knows about the charge of a payment, with status ChargeNotFound when the
	// provider never received it. An error means that the provider could not be asked.
	Status(ctx context.Context, req StatusRequest) (Charge, error)
}

// ErrUnavailable is wrapped by Pay errors that guarantee nothing was charged: the provider refused to take the
// request, for example because it is down for maintenance or rate-limits the caller. A later retry may succeed.
var ErrUnavailable = errors.New("payment provider unavailable")

// validID is the shape of provider ids: short, lowercase, safe in URLs, logs, and configuration keys.
var validID = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// ValidID reports whether id can serve as a provider id.
func ValidID(id string) bool {
	return validID.MatchString(id)
}

// PayRequest asks a provider to charge a payment.
type PayRequest struct {
	PaymentID   uuid.UUID // the idempotency key
	BookingID   uuid.UUID // for the provider's records, such as a statement description
	AmountCents int64
	Currency    string // ISO 4217 code
	Token       string // stands for the payment source at the provider, such as a tokenized card
}

// RefundRequest asks a provider to refund the charge of a payment.
type RefundRequest struct {
	PaymentID   uuid.UUID
	ProviderRef string // the provider's id of the charge
	AmountCents int64
	Currency    string
}

// StatusRequest asks a provider about the charge of a payment whose outcome the caller does not know.
type StatusRequest struct {
	PaymentID uuid.UUID
}

// ChargeStatus is a provider's definite answer about a charge.
type ChargeStatus string

// Charge statuses. Pay answers ChargeSucceeded or ChargeDeclined; Status may answer any of them.
const (
	ChargeSucceeded ChargeStatus = "succeeded" // the money was taken
	ChargeDeclined  ChargeStatus = "declined"  // the provider refused the charge; nothing was taken
	ChargeRefunded  ChargeStatus = "refunded"  // the money was taken and returned
	ChargeNotFound  ChargeStatus = "not_found" // the provider has no charge for this payment
)

// Decline codes that adapters map their provider's reasons to, where one fits. A provider may answer other
// short snake_case codes.
const (
	DeclineCardDeclined      = "card_declined"
	DeclineInsufficientFunds = "insufficient_funds"
	DeclineExpiredCard       = "expired_card"
	DeclineInvalidToken      = "invalid_token"
)

// Charge is what a provider knows about the charge of one payment.
type Charge struct {
	Status      ChargeStatus
	Ref         string // the provider's id of the charge; empty when there is none
	DeclineCode string // why the charge was declined, such as DeclineInsufficientFunds
}
