// Package httpapi assembles the HTTP API: routes, middleware, and handlers.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/onetodone/cinema-api/internal/transport/httpapi/handler"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/middleware"
)

// RouterDeps holds everything the router needs to build its handlers.
type RouterDeps struct {
	Logger *slog.Logger
	Health *handler.Health
}

// NewRouter registers all routes and wraps them in the shared middleware stack.
func NewRouter(d RouterDeps) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", d.Health.Live)
	mux.HandleFunc("GET /readyz", d.Health.Ready)

	// Order matters: RequestID is outermost so every log line carries the ID, and Recover is innermost so that a
	// recovered panic is still logged by AccessLog with its 500 status.
	return middleware.Chain(mux,
		middleware.RequestID,
		middleware.AccessLog(d.Logger),
		middleware.Recover(d.Logger),
	)
}
