package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

// writeError maps err to a problem response. Server-side failures are logged with the full error, which
// the response never contains.
func writeError(logger *slog.Logger, w http.ResponseWriter, r *http.Request, err error) {
	p := problem.FromError(err)
	if p.Status >= http.StatusInternalServerError {
		level := slog.LevelError
		if errors.Is(err, context.Canceled) {
			level = slog.LevelInfo // the client went away; nothing is broken
		}
		logger.Log(r.Context(), level, "request failed", slog.Any("error", err))
	}
	problem.Write(w, r, p)
}
