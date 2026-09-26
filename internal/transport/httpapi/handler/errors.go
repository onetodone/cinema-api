package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/principal"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

// writeError maps err to a problem response. Server-side failures are logged with the full error, which
// the response never contains.
func writeError(logger *slog.Logger, w http.ResponseWriter, r *http.Request, err error) {
	p := problem.FromError(err)
	if p.Status >= http.StatusInternalServerError {
		level := slog.LevelError
		switch {
		case errors.Is(err, context.Canceled):
			level = slog.LevelInfo // the client went away; nothing is broken
		case errors.Is(err, domain.ErrUnavailable):
			level = slog.LevelWarn // a dependency refused the request; this service works as intended
		}
		logger.Log(r.Context(), level, "request failed", slog.Any("error", err))
	}
	problem.Write(w, r, p)
}

// caller returns the authenticated caller of r. Without one it writes a 500 and returns false: the route is
// missing the Authenticate middleware, which is a wiring bug, and failing closed keeps the handler from acting
// for an anonymous caller.
func caller(logger *slog.Logger, w http.ResponseWriter, r *http.Request) (domain.Principal, bool) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(logger, w, r, fmt.Errorf("%s %s is routed without authentication", r.Method, r.URL.Path))
	}
	return p, ok
}
