package handler

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

// params collects request-parameter errors, so one response can report every invalid parameter at once.
type params struct {
	errs []problem.FieldError
}

func (p *params) fail(field, format string, args ...any) {
	p.errs = append(p.errs, problem.FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}

// ok writes a 400 validation problem and returns false if any parameter was invalid.
func (p *params) ok(w http.ResponseWriter, r *http.Request) bool {
	if len(p.errs) == 0 {
		return true
	}
	problem.Write(w, r, problem.Validation(p.errs...))
	return false
}

// pathID parses a positive integer path parameter.
func (p *params) pathID(r *http.Request, name string) int64 {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		p.fail(name, "must be a positive integer")
		return 0
	}
	return id
}

// pathUUID parses a UUID path parameter.
func (p *params) pathUUID(r *http.Request, name string) uuid.UUID {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		p.fail(name, "must be a UUID")
		return uuid.UUID{}
	}
	return id
}

// optionalID parses an optional positive integer query parameter; absent means 0.
func (p *params) optionalID(r *http.Request, name string) int64 {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		p.fail(name, "must be a positive integer")
		return 0
	}
	return id
}

// limit parses the optional page size, at most maxSize; absent means the service default.
func (p *params) limit(r *http.Request, maxSize int) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxSize {
		p.fail("limit", "must be an integer between 1 and %d", maxSize)
		return 0
	}
	return n
}

// cursor decodes the opaque pagination cursor; absent means the first page.
func (p *params) cursor(r *http.Request) int64 {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return 0
	}
	id, err := decodeCursor(raw)
	if err != nil {
		p.fail("cursor", "is not a valid cursor")
		return 0
	}
	return id
}

// uuidCursor decodes an opaque pagination cursor that wraps a UUID; absent means the first page.
func (p *params) uuidCursor(r *http.Request) uuid.UUID {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return uuid.UUID{}
	}
	id, err := decodeUUIDCursor(raw)
	if err != nil {
		p.fail("cursor", "is not a valid cursor")
		return uuid.UUID{}
	}
	return id
}

// date parses an optional YYYY-MM-DD query parameter; absent means the zero time.
func (p *params) date(r *http.Request, name string) time.Time {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}
	}
	d, err := time.Parse(time.DateOnly, raw)
	if err != nil {
		p.fail(name, "must be a date in YYYY-MM-DD format")
		return time.Time{}
	}
	return d
}

// Cursors are opaque to clients: base64url of the last id. Opaque cursors let the pagination key change later
// without breaking clients.
func encodeCursor(afterID int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(afterID, 10)))
}

func decodeCursor(s string) (int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, err
	}
	id, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, err
	}
	if id <= 0 {
		return 0, fmt.Errorf("cursor id %d is not positive", id)
	}
	return id, nil
}

func encodeUUIDCursor(id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id.String()))
}

func decodeUUIDCursor(s string) (uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return uuid.UUID{}, err
	}
	return uuid.Parse(string(raw))
}
