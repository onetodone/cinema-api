package middleware

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/onetodone/cinema-api/internal/transport/httpapi/render"
)

// Recover turns a panic in a handler into a 500 response and logs the panic with its stack trace.
// http.ErrAbortHandler is re-raised, because net/http uses it to abort a response on purpose.
func Recover(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer recoverPanic(logger, w, r)
			next.ServeHTTP(w, r)
		})
	}
}

// recoverPanic must be called directly by defer, otherwise recover() returns nil.
func recoverPanic(logger *slog.Logger, w http.ResponseWriter, r *http.Request) {
	rec := recover()
	if rec == nil {
		return
	}
	if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(rec)
	}

	logger.ErrorContext(r.Context(), "panic recovered",
		slog.Any("panic", rec),
		slog.String("stack", string(debug.Stack())),
	)
	render.JSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
}
