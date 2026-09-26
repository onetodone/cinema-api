// Package problem writes RFC 9457 "problem details" error responses and maps errors to them.
package problem

import (
	"errors"
	"net/http"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/render"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/requestid"
)

// ContentType is the media type of problem details (RFC 9457).
const ContentType = "application/problem+json"

// Codes for errors that do not come from the domain.
const (
	CodeValidationFailed = "VALIDATION_FAILED"
	CodeNotFound         = "NOT_FOUND"
	CodeMethodNotAllowed = "METHOD_NOT_ALLOWED"
	CodeInternal         = "INTERNAL"
)

// FieldError describes one invalid request parameter.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Problem is an RFC 9457 problem details object. Type is "about:blank", so Title is the HTTP status phrase;
// clients branch on Code, which is stable across releases.
type Problem struct {
	Type      string       `json:"type"`
	Title     string       `json:"title"`
	Status    int          `json:"status"`
	Detail    string       `json:"detail,omitempty"`
	Instance  string       `json:"instance,omitempty"`
	Code      string       `json:"code"`
	RequestID string       `json:"request_id,omitempty"`
	Errors    []FieldError `json:"errors,omitempty"`
}

// New returns a problem for an HTTP status with a machine-readable code and a human-readable detail.
func New(status int, code, detail string) Problem {
	return Problem{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
		Code:   code,
	}
}

// Validation returns a 400 problem that lists the invalid parameters.
func Validation(errs ...FieldError) Problem {
	p := New(http.StatusBadRequest, CodeValidationFailed, "The request has invalid parameters.")
	p.Errors = errs
	return p
}

// Internal returns a 500 problem without any detail about the cause.
func Internal() Problem {
	return New(http.StatusInternalServerError, CodeInternal, "An unexpected error occurred.")
}

// FromError maps err to a problem. Domain errors keep their code and client-safe message. Any other error
// becomes a generic 500, so internal details never reach clients.
func FromError(err error) Problem {
	var de *domain.Error
	if !errors.As(err, &de) {
		return Internal()
	}

	switch {
	case errors.Is(de.Kind, domain.ErrNotFound):
		return New(http.StatusNotFound, de.Code, de.Message)
	case errors.Is(de.Kind, domain.ErrConflict):
		return New(http.StatusConflict, de.Code, de.Message)
	case errors.Is(de.Kind, domain.ErrInvalid):
		return New(http.StatusUnprocessableEntity, de.Code, de.Message)
	default:
		return Internal()
	}
}

// Write sends p, filling in the request path as the instance and the request ID.
func Write(w http.ResponseWriter, r *http.Request, p Problem) {
	if p.Instance == "" {
		p.Instance = r.URL.Path
	}
	if p.RequestID == "" {
		p.RequestID = requestid.FromContext(r.Context())
	}
	render.Encode(w, p.Status, ContentType, p)
}
