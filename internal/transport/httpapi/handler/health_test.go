package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var errDown = errors.New("connection refused")

func up(context.Context) error   { return nil }
func down(context.Context) error { return errDown }

func blockUntilDone(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func serveReady(t *testing.T, h *Health) (int, readinessResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))

	var body readinessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	return rec.Code, body
}

func TestLive(t *testing.T) {
	t.Parallel()

	h := NewHealth(slog.New(slog.DiscardHandler), time.Second, Check{Name: "postgres", Critical: true, Probe: down})
	rec := httptest.NewRecorder()
	h.Live(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 even when dependencies are down", rec.Code)
	}
	if got := rec.Body.String(); got != "{\"status\":\"ok\"}\n" {
		t.Errorf("body = %q", got)
	}
}

func TestReady(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		postgres   func(context.Context) error
		redis      func(context.Context) error
		wantCode   int
		wantStatus string
		wantChecks map[string]string
	}{
		{
			name:       "all dependencies up",
			postgres:   up,
			redis:      up,
			wantCode:   http.StatusOK,
			wantStatus: StatusOK,
			wantChecks: map[string]string{"postgres": "up", "redis": "up"},
		},
		{
			name:       "non-critical redis down degrades but stays ready",
			postgres:   up,
			redis:      down,
			wantCode:   http.StatusOK,
			wantStatus: StatusDegraded,
			wantChecks: map[string]string{"postgres": "up", "redis": "down"},
		},
		{
			name:       "critical postgres down makes the service unavailable",
			postgres:   down,
			redis:      up,
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: StatusUnavailable,
			wantChecks: map[string]string{"postgres": "down", "redis": "up"},
		},
		{
			name:       "everything down is unavailable, not degraded",
			postgres:   down,
			redis:      down,
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: StatusUnavailable,
			wantChecks: map[string]string{"postgres": "down", "redis": "down"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := NewHealth(slog.New(slog.DiscardHandler), time.Second,
				Check{Name: "postgres", Critical: true, Probe: tt.postgres},
				Check{Name: "redis", Critical: false, Probe: tt.redis},
			)
			code, body := serveReady(t, h)

			if code != tt.wantCode {
				t.Errorf("status code = %d, want %d", code, tt.wantCode)
			}
			if body.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", body.Status, tt.wantStatus)
			}
			for name, want := range tt.wantChecks {
				if body.Checks[name] != want {
					t.Errorf("checks[%s] = %q, want %q", name, body.Checks[name], want)
				}
			}
		})
	}
}

func TestReadyTimesOutHangingProbes(t *testing.T) {
	t.Parallel()

	h := NewHealth(slog.New(slog.DiscardHandler), 50*time.Millisecond,
		Check{Name: "postgres", Critical: true, Probe: blockUntilDone},
	)

	start := time.Now()
	code, body := serveReady(t, h)

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("readiness took %s, want it bounded by the timeout", elapsed)
	}
	if code != http.StatusServiceUnavailable || body.Checks["postgres"] != "down" {
		t.Errorf("got %d %+v, want 503 with postgres down", code, body)
	}
}

func TestReadyDoesNotLeakErrorDetails(t *testing.T) {
	t.Parallel()

	h := NewHealth(slog.New(slog.DiscardHandler), time.Second, Check{Name: "postgres", Critical: true, Probe: down})
	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))

	if body := rec.Body.String(); strings.Contains(body, errDown.Error()) {
		t.Errorf("body leaks the probe error: %q", body)
	}
}
