package middleware

import (
	"log/slog"
	"net/http"
	"uuid"

	"github.com/onetodone/cinema-api/internal/platform/logging"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/requestid"
)

// RequestIDHeader is the header used to receive and return the request ID.
const RequestIDHeader = "X-Request-ID"

const maxRequestIDLength = 128

// RequestID assigns every request an ID. A well-formed incoming X-Request-ID is reused so that IDs can be traced
// across services; otherwise a UUIDv7 is generated. The ID is returned in the response header, stored in the
// request context (read it with requestid.FromContext), and attached to every log record written with that context.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID(id) {
			id = uuid.NewV7().String()
		}

		w.Header().Set(RequestIDHeader, id)
		ctx := requestid.NewContext(r.Context(), id)
		ctx = logging.WithAttrs(ctx, slog.String("request_id", id))

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// validRequestID accepts short IDs made of URL-safe characters only, so a client cannot inject arbitrary
// content into logs or response headers.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLength {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}
