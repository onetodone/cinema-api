// Package handler contains the HTTP handlers.
package handler

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/onetodone/cinema-api/internal/transport/httpapi/render"
)

// Readiness statuses reported by /readyz.
const (
	StatusOK          = "ok"
	StatusDegraded    = "degraded"
	StatusUnavailable = "unavailable"
)

// Check is one dependency probe used by the readiness endpoint.
type Check struct {
	Name string
	// Critical marks a dependency the service cannot work without. A failing critical check makes /readyz
	// return 503; a failing non-critical check (such as Redis, which is fail-open) only reports "degraded".
	Critical bool
	Probe    func(ctx context.Context) error
}

// Health serves the liveness and readiness endpoints.
type Health struct {
	logger  *slog.Logger
	timeout time.Duration
	checks  []Check
}

// NewHealth returns a Health handler that runs checks with the given overall timeout.
func NewHealth(logger *slog.Logger, timeout time.Duration, checks ...Check) *Health {
	return &Health{logger: logger, timeout: timeout, checks: checks}
}

type livenessResponse struct {
	Status string `json:"status"`
}

type readinessResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// Live reports that the process is running. It never touches dependencies, so a slow database cannot make the
// orchestrator restart healthy processes.
func (h *Health) Live(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	render.JSON(w, http.StatusOK, livenessResponse{Status: StatusOK})
}

// Ready runs all checks concurrently and reports whether the service can take traffic.
// Failure details are logged, not returned, so the endpoint does not leak infrastructure information.
func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	results := make([]error, len(h.checks))
	var wg sync.WaitGroup
	for i, c := range h.checks {
		wg.Go(func() { results[i] = c.Probe(ctx) })
	}
	wg.Wait()

	resp := readinessResponse{Status: StatusOK, Checks: make(map[string]string, len(h.checks))}
	code := http.StatusOK
	for i, c := range h.checks {
		if results[i] == nil {
			resp.Checks[c.Name] = "up"
			continue
		}

		resp.Checks[c.Name] = "down"
		h.logger.WarnContext(r.Context(), "readiness check failed",
			slog.String("check", c.Name),
			slog.Bool("critical", c.Critical),
			slog.Any("error", results[i]),
		)
		switch {
		case c.Critical:
			resp.Status = StatusUnavailable
			code = http.StatusServiceUnavailable
		case resp.Status == StatusOK:
			resp.Status = StatusDegraded
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	render.JSON(w, code, resp)
}
