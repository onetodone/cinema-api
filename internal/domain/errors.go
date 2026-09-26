// Package domain holds the business entities, rules, and errors. It has no infrastructure dependencies.
package domain

import (
	"errors"
	"fmt"
	"strings"
)

// Error kinds. Use errors.Is(err, domain.ErrNotFound) to branch on the kind of a domain error.
var (
	ErrNotFound        = errors.New("not found")
	ErrConflict        = errors.New("conflict")
	ErrInvalid         = errors.New("invalid")
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrForbidden       = errors.New("forbidden")
	// ErrBusy means another operation holds a lock on the same data. The request itself is fine and is likely
	// to succeed when retried shortly.
	ErrBusy = errors.New("busy")
	// ErrGone means the target can never be acted on again, such as a booking whose hold has run out.
	ErrGone = errors.New("gone")
	// ErrPaymentRequired means a payment provider refused the charge. Another payment source may succeed.
	ErrPaymentRequired = errors.New("payment required")
	// ErrUnavailable means a service the request depends on cannot take it right now. Nothing was changed, and
	// a later retry may succeed.
	ErrUnavailable = errors.New("unavailable")
)

// Stable, machine-readable error codes returned to API clients.
const (
	CodeMovieNotFound    = "MOVIE_NOT_FOUND"
	CodeHallNotFound     = "HALL_NOT_FOUND"
	CodeShowtimeNotFound = "SHOWTIME_NOT_FOUND"
	CodeHallOverlap      = "HALL_OVERLAP"
	CodeHallNameTaken    = "HALL_NAME_TAKEN"

	CodeUserNotFound       = "USER_NOT_FOUND"
	CodeEmailTaken         = "EMAIL_TAKEN"
	CodeInvalidCredentials = "INVALID_CREDENTIALS" //nolint:gosec // G101: an error code, not a credential
	CodeUnauthenticated    = "UNAUTHENTICATED"
	CodeInvalidToken       = "INVALID_TOKEN"
	CodeTokenExpired       = "TOKEN_EXPIRED"
	CodeForbidden          = "FORBIDDEN"
	// CodeRefreshInvalid answers every refresh that fails, without saying why: an unknown, expired, revoked, or
	// reused refresh token all look the same to the client.
	CodeRefreshInvalid  = "REFRESH_INVALID"
	CodeSessionNotFound = "SESSION_NOT_FOUND"

	CodeBookingNotFound      = "BOOKING_NOT_FOUND"
	CodeShowtimeNotBookable  = "SHOWTIME_NOT_BOOKABLE"
	CodeUnknownSeat          = "UNKNOWN_SEAT"
	CodeSeatUnavailable      = "SEAT_UNAVAILABLE"
	CodeSeatBusy             = "SEAT_BUSY"
	CodeBookingBusy          = "BOOKING_BUSY"
	CodeActiveBookingExists  = "ACTIVE_BOOKING_EXISTS"
	CodeBookingNotCancelable = "BOOKING_NOT_CANCELABLE"

	CodePaymentMethodUnavailable   = "PAYMENT_METHOD_UNAVAILABLE"
	CodePaymentInProgress          = "PAYMENT_IN_PROGRESS"
	CodePaymentDeclined            = "PAYMENT_DECLINED"
	CodePaymentProviderUnavailable = "PAYMENT_PROVIDER_UNAVAILABLE"
	CodePaymentRefunded            = "PAYMENT_REFUNDED"
	CodeBookingExpired             = "BOOKING_EXPIRED"
	CodeBookingAlreadyPaid         = "BOOKING_ALREADY_PAID"
	CodeBookingCanceled            = "BOOKING_CANCELED"
)

// Error is a business error. Its message is safe to show to API clients; Code is stable across releases.
type Error struct {
	Kind    error
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// Unwrap exposes the kind, so errors.Is(err, ErrNotFound) works through any wrapping.
func (e *Error) Unwrap() error { return e.Kind }

// NotFound builds an ErrNotFound error.
func NotFound(code, format string, args ...any) error {
	return &Error{Kind: ErrNotFound, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Conflict builds an ErrConflict error.
func Conflict(code, format string, args ...any) error {
	return &Error{Kind: ErrConflict, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Invalid builds an ErrInvalid error.
func Invalid(code, format string, args ...any) error {
	return &Error{Kind: ErrInvalid, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Unauthenticated builds an ErrUnauthenticated error: the caller's identity is missing or not proven.
func Unauthenticated(code, format string, args ...any) error {
	return &Error{Kind: ErrUnauthenticated, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Forbidden builds an ErrForbidden error: the caller is known but not allowed to do this.
func Forbidden(code, format string, args ...any) error {
	return &Error{Kind: ErrForbidden, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Busy builds an ErrBusy error: the data is locked by a concurrent operation, and a retry will likely succeed.
func Busy(code, format string, args ...any) error {
	return &Error{Kind: ErrBusy, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Gone builds an ErrGone error: the target can never be acted on again.
func Gone(code, format string, args ...any) error {
	return &Error{Kind: ErrGone, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Unavailable builds an ErrUnavailable error: a service the request depends on cannot take it right now.
func Unavailable(code, format string, args ...any) error {
	return &Error{Kind: ErrUnavailable, Code: code, Message: fmt.Sprintf(format, args...)}
}

// FieldError explains why one input field is invalid. Field is the API's name for it, such as "email".
type FieldError struct {
	Field   string
	Message string
}

// ValidationError reports every invalid field of an input at once. It is of kind ErrInvalid.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Field+" "+f.Message)
	}
	return "invalid input: " + strings.Join(parts, "; ")
}

// Unwrap exposes the kind, so errors.Is(err, ErrInvalid) holds for validation errors too.
func (e *ValidationError) Unwrap() error { return ErrInvalid }

// Violations collects field errors while an input is checked. The zero value is ready to use.
type Violations struct {
	fields []FieldError
}

// Add records that field is invalid. The message reads after the field name, such as "is required".
func (v *Violations) Add(field, format string, args ...any) {
	v.fields = append(v.fields, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}

// Check records err as the reason field is invalid, if err is not nil.
func (v *Violations) Check(field string, err error) {
	if err != nil {
		v.fields = append(v.fields, FieldError{Field: field, Message: err.Error()})
	}
}

// Err returns a *ValidationError listing every recorded field, or nil if there were none.
func (v *Violations) Err() error {
	if len(v.fields) == 0 {
		return nil
	}
	return &ValidationError{Fields: v.fields}
}
