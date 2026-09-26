package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/onetodone/cinema-api/internal/platform/logging"
)

func newJSONLogger(t *testing.T, buf *bytes.Buffer) *slog.Logger {
	t.Helper()
	logger, err := logging.New(buf, slog.LevelDebug, "json")
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	return logger
}

func decodeLogLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rec); err != nil {
		t.Fatalf("decode log line %q: %v", buf.String(), err)
	}
	return rec
}

func TestChainOrder(t *testing.T) {
	t.Parallel()

	var order []string
	mark := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { order = append(order, "handler") }),
		mark("outer"), mark("inner"))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	if got := strings.Join(order, ","); got != "outer,inner,handler" {
		t.Errorf("order = %s, want outer,inner,handler", got)
	}
}

func TestRequestID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		incoming string
		reuse    bool
	}{
		{name: "generated when missing", incoming: "", reuse: false},
		{name: "reused when well formed", incoming: "trace-abc_123.x:y", reuse: true},
		{name: "replaced when too long", incoming: strings.Repeat("a", maxRequestIDLength+1), reuse: false},
		{name: "replaced when it has unsafe characters", incoming: "evil\nlog line", reuse: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var seen string
			h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen = RequestIDFromContext(r.Context())
			}))

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			if tt.incoming != "" {
				req.Header.Set(RequestIDHeader, tt.incoming)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			header := rec.Header().Get(RequestIDHeader)
			if header == "" || header != seen {
				t.Fatalf("header %q and context %q must match and be non-empty", header, seen)
			}
			if tt.reuse {
				if header != tt.incoming {
					t.Errorf("request ID = %q, want incoming %q", header, tt.incoming)
				}
				return
			}
			if _, err := uuid.Parse(header); err != nil {
				t.Errorf("generated request ID %q is not a UUID: %v", header, err)
			}
		})
	}
}

func TestRequestIDFromContextWithoutMiddleware(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	if got := RequestIDFromContext(req.Context()); got != "" {
		t.Errorf("RequestIDFromContext = %q, want empty", got)
	}
}

func TestAccessLogRecordsRequest(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := newJSONLogger(t, &buf)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/things/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("short and stout"))
	})
	h := Chain(mux, RequestID, AccessLog(logger))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/things/42", nil)
	req.Header.Set(RequestIDHeader, "req-42")
	h.ServeHTTP(httptest.NewRecorder(), req)

	rec := decodeLogLine(t, &buf)
	want := map[string]any{
		"level":      "INFO",
		"msg":        "http request",
		"method":     "GET",
		"path":       "/v1/things/42",
		"route":      "GET /v1/things/{id}",
		"status":     float64(http.StatusTeapot),
		"bytes":      float64(len("short and stout")),
		"request_id": "req-42",
	}
	for key, value := range want {
		if rec[key] != value {
			t.Errorf("%s = %v, want %v", key, rec[key], value)
		}
	}
	if ms, ok := rec["duration_ms"].(float64); !ok || ms < 0 {
		t.Errorf("duration_ms = %v, want a non-negative number", rec["duration_ms"])
	}
}

func TestAccessLogLevels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		path   string
		status int
		level  string
	}{
		{name: "implicit 200", path: "/v1/movies", status: 0, level: "INFO"},
		{name: "server error", path: "/v1/movies", status: http.StatusBadGateway, level: "ERROR"},
		{name: "health probe", path: "/healthz", status: http.StatusOK, level: "DEBUG"},
		{name: "failing readiness probe", path: "/readyz", status: http.StatusServiceUnavailable, level: "ERROR"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			h := AccessLog(newJSONLogger(t, &buf))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				}
			}))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil))

			rec := decodeLogLine(t, &buf)
			if rec["level"] != tt.level {
				t.Errorf("level = %v, want %s", rec["level"], tt.level)
			}
			wantStatus := tt.status
			if wantStatus == 0 {
				wantStatus = http.StatusOK
			}
			if rec["status"] != float64(wantStatus) {
				t.Errorf("status = %v, want %d", rec["status"], wantStatus)
			}
		})
	}
}

func TestStatusRecorderSupportsResponseController(t *testing.T) {
	t.Parallel()

	var flushErr error
	h := AccessLog(slog.New(slog.DiscardHandler))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flushErr = http.NewResponseController(w).Flush()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	if flushErr != nil {
		t.Errorf("Flush through the recorder failed: %v", flushErr)
	}
}

func TestRecoverTurnsPanicInto500(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }),
		RequestID, Recover(newJSONLogger(t, &buf)))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("response body leaks the panic value: %q", rec.Body.String())
	}

	logged := decodeLogLine(t, &buf)
	if logged["msg"] != "panic recovered" || logged["panic"] != "boom" {
		t.Errorf("log = %v, want the recovered panic", logged)
	}
	if stack, _ := logged["stack"].(string); !strings.Contains(stack, "runtime/debug.Stack") {
		t.Errorf("log has no stack trace: %v", logged["stack"])
	}
	if logged["request_id"] == nil {
		t.Errorf("log has no request_id: %v", logged)
	}
}

func TestRecoverReraisesAbortHandler(t *testing.T) {
	t.Parallel()

	h := Recover(slog.New(slog.DiscardHandler))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler { //nolint:errorlint // identity check on the re-raised sentinel
			t.Errorf("recovered %v, want http.ErrAbortHandler to be re-raised", rec)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	t.Error("ServeHTTP returned normally, want a panic")
}
