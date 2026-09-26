// Package domain holds the business entities, rules, and errors. It has no infrastructure dependencies.
package domain

import (
	"errors"
	"fmt"
)

// Error kinds. Use errors.Is(err, domain.ErrNotFound) to branch on the kind of a domain error.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrInvalid  = errors.New("invalid")
)

// Stable, machine-readable error codes returned to API clients.
const (
	CodeMovieNotFound    = "MOVIE_NOT_FOUND"
	CodeHallNotFound     = "HALL_NOT_FOUND"
	CodeShowtimeNotFound = "SHOWTIME_NOT_FOUND"
	CodeHallOverlap      = "HALL_OVERLAP"
	CodeHallNameTaken    = "HALL_NAME_TAKEN"
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
