package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/principal"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

type memRecord struct {
	owner  string
	record []byte
	ttl    time.Duration
}

// memStore is an in-memory IdempotencyStore that ignores expiry. A non-nil err makes every call fail.
type memStore struct {
	mu       sync.Mutex
	records  map[string]memRecord
	err      error
	stale    bool  // Complete reports that the key has another owner
	ctxErrs  []any // ctx.Err() of every Complete and Release call
	releases int
}

func newMemStore() *memStore { return &memStore{records: map[string]memRecord{}} }

func (s *memStore) Claim(_ context.Context, key, owner string, record []byte, ttl time.Duration) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, false, s.err
	}
	if r, ok := s.records[key]; ok {
		return r.record, false, nil
	}
	s.records[key] = memRecord{owner: owner, record: record, ttl: ttl}
	return nil, true, nil
}

func (s *memStore) Complete(ctx context.Context, key, owner string, record []byte, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctxErrs = append(s.ctxErrs, ctx.Err())
	if s.err != nil || s.stale {
		return false, s.err
	}
	if r, ok := s.records[key]; !ok || r.owner != owner {
		return false, nil
	}
	s.records[key] = memRecord{owner: owner, record: record, ttl: ttl}
	return true, nil
}

func (s *memStore) Release(ctx context.Context, key, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctxErrs = append(s.ctxErrs, ctx.Err())
	s.releases++
	if r, ok := s.records[key]; ok && r.owner == owner {
		delete(s.records, key)
	}
	return s.err
}

func (s *memStore) only(t *testing.T) (memRecord, idempotencyRecord) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.records) != 1 {
		t.Fatalf("store holds %d records, want 1", len(s.records))
	}
	for _, r := range s.records {
		var rec idempotencyRecord
		if err := json.Unmarshal(r.record, &rec); err != nil {
			t.Fatal(err)
		}
		return r, rec
	}
	panic("unreachable")
}

// countingHandler answers with the given status and a body that numbers the call.
type countingHandler struct {
	mu     sync.Mutex
	calls  int
	status int
	header http.Header
	bodies []string // request bodies it read
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	h.calls++
	n := h.calls
	h.bodies = append(h.bodies, string(body))
	h.mu.Unlock()
	for k, v := range h.header {
		w.Header()[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Delivery", "not replayed")
	w.WriteHeader(h.status)
	_, _ = w.Write([]byte(`{"call":` + string(rune('0'+n)) + `}`))
}

var ann = domain.Principal{UserID: uuid.NewV7(), Role: domain.RoleCustomer}

// idemRequest builds a POST from caller (none if zero) with an Idempotency-Key (none if empty).
func idemRequest(t *testing.T, caller domain.Principal, path, key, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set(IdempotencyKeyHeader, key)
	}
	if caller != (domain.Principal{}) {
		r = r.WithContext(principal.NewContext(r.Context(), caller))
	}
	return r
}

func idempotencyCount(m *metrics.Metrics, result string) float64 {
	return testutil.ToFloat64(m.IdempotencyRequests.WithLabelValues(result))
}

func TestIdempotencyReplaysTheFirstResponse(t *testing.T) {
	t.Parallel()

	store, m := newMemStore(), newMetrics()
	next := &countingHandler{status: http.StatusCreated, header: http.Header{"Location": {"/v1/bookings/1"}}}
	h := Idempotency(store, false, m, slog.New(slog.DiscardHandler))(next)

	first := httptest.NewRecorder()
	h.ServeHTTP(first, idemRequest(t, ann, "/v1/bookings", "k1", `{"a":1}`))
	again := httptest.NewRecorder()
	h.ServeHTTP(again, idemRequest(t, ann, "/v1/bookings", "k1", `{"a":1}`))

	if next.calls != 1 || next.bodies[0] != `{"a":1}` {
		t.Errorf("handler ran %d times with bodies %q, want once with the request body", next.calls, next.bodies)
	}
	if again.Code != http.StatusCreated || again.Body.String() != first.Body.String() {
		t.Errorf("replay = %d %s, want %d %s", again.Code, again.Body, first.Code, first.Body)
	}
	if again.Header().Get(IdempotentReplayedHeader) != "true" || first.Header().Get(IdempotentReplayedHeader) != "" {
		t.Errorf("Idempotent-Replayed: first %q, replay %q", first.Header().Get(IdempotentReplayedHeader),
			again.Header().Get(IdempotentReplayedHeader))
	}
	if again.Header().Get("Location") != "/v1/bookings/1" || again.Header().Get("Content-Type") != "application/json" ||
		again.Header().Get("X-Delivery") != "" {
		t.Errorf("replayed headers = %v, want Location and Content-Type only", again.Header())
	}
	raw, rec := store.only(t)
	if !rec.Done || rec.Status != http.StatusCreated || raw.ttl != IdempotencyTTL {
		t.Errorf("stored record = %+v with TTL %s", rec, raw.ttl)
	}
	if idempotencyCount(m, metrics.IdempotencyNew) != 1 || idempotencyCount(m, metrics.IdempotencyReplayed) != 1 {
		t.Error("metrics do not show one new and one replayed request")
	}
}

func TestIdempotencyKeysBelongToCallerMethodAndPath(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	next := &countingHandler{status: http.StatusOK}
	h := Idempotency(store, false, newMetrics(), slog.New(slog.DiscardHandler))(next)
	bob := domain.Principal{UserID: uuid.NewV7(), Role: domain.RoleCustomer}

	for _, r := range []*http.Request{
		idemRequest(t, ann, "/v1/bookings/1/payments", "k1", `{}`),
		idemRequest(t, bob, "/v1/bookings/1/payments", "k1", `{}`),
		idemRequest(t, ann, "/v1/bookings/2/payments", "k1", `{}`),
	} {
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if next.calls != 3 {
		t.Errorf("handler ran %d times, want 3: the key is scoped to the caller and the path", next.calls)
	}
}

func TestIdempotencyRejects(t *testing.T) {
	t.Parallel()

	claimed := func(s *memStore, fingerprint string, done bool) {
		raw, _ := json.Marshal(idempotencyRecord{Fingerprint: fingerprint, Done: done, Status: 200})
		s.records[ann.UserID.String()+":"+digest("POST /v1/x\nk1")] = memRecord{owner: "other", record: raw}
	}
	tests := []struct {
		name     string
		required bool
		key      string
		setup    func(s *memStore)
		status   int
		code     string
		result   string
	}{
		{name: "missing required key", required: true, status: http.StatusBadRequest, code: problem.CodeValidationFailed},
		{name: "key too long", key: strings.Repeat("k", 256), status: http.StatusBadRequest, code: problem.CodeValidationFailed},
		{name: "key with a control character", key: "k\x01", status: http.StatusBadRequest, code: problem.CodeValidationFailed},
		{
			name: "first request still running", key: "k1", setup: func(s *memStore) { claimed(s, digest(`{"a":1}`), false) },
			status: http.StatusConflict, code: problem.CodeIdempotencyInProgress, result: metrics.IdempotencyInProgress,
		},
		{
			name: "key reused with another body", key: "k1", setup: func(s *memStore) { claimed(s, digest(`{"a":2}`), true) },
			status: http.StatusUnprocessableEntity, code: problem.CodeIdempotencyKeyReused, result: metrics.IdempotencyKeyReused,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, m := newMemStore(), newMetrics()
			if tt.setup != nil {
				tt.setup(store)
			}
			var called bool
			h := Idempotency(store, tt.required, m, slog.New(slog.DiscardHandler))(okHandler(&called))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, idemRequest(t, ann, "/v1/x", tt.key, `{"a":1}`))

			if rec.Code != tt.status || !strings.Contains(rec.Body.String(), `"code":"`+tt.code+`"`) || called {
				t.Errorf("got %d %s (handler called %t), want %d %s", rec.Code, rec.Body, called, tt.status, tt.code)
			}
			if tt.status == http.StatusBadRequest && !strings.Contains(rec.Body.String(), `"field":"Idempotency-Key"`) {
				t.Errorf("body = %s, want the header named as the invalid field", rec.Body)
			}
			if tt.status == http.StatusConflict && rec.Header().Get("Retry-After") != "1" {
				t.Errorf("Retry-After = %q, want 1", rec.Header().Get("Retry-After"))
			}
			if tt.result != "" && idempotencyCount(m, tt.result) != 1 {
				t.Errorf("metric %s not counted", tt.result)
			}
		})
	}
}

func TestIdempotencyFreesTheKeyAfterTransientFailures(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		status int
		header http.Header
	}{
		{name: "server error", status: http.StatusInternalServerError},
		{name: "unavailable dependency", status: http.StatusServiceUnavailable, header: http.Header{"Retry-After": {"5"}}},
		{name: "busy seat", status: http.StatusConflict, header: http.Header{"Retry-After": {"1"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := newMemStore()
			next := &countingHandler{status: tt.status, header: tt.header}
			h := Idempotency(store, true, newMetrics(), slog.New(slog.DiscardHandler))(next)
			for range 2 {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, idemRequest(t, ann, "/v1/x", "k1", `{}`))
				if rec.Code != tt.status {
					t.Fatalf("status = %d, want %d", rec.Code, tt.status)
				}
			}
			if next.calls != 2 || len(store.records) != 0 {
				t.Errorf("handler ran %d times, %d records left; want 2 runs and no record", next.calls, len(store.records))
			}
		})
	}
}

func TestIdempotencyStoresClientErrors(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	next := &countingHandler{status: http.StatusPaymentRequired}
	h := Idempotency(store, true, newMetrics(), slog.New(slog.DiscardHandler))(next)
	for range 2 {
		h.ServeHTTP(httptest.NewRecorder(), idemRequest(t, ann, "/v1/x", "k1", `{}`))
	}
	if next.calls != 1 {
		t.Errorf("handler ran %d times, want once: a decline is the final answer to this request", next.calls)
	}
}

func TestIdempotencyReleasesTheKeyWhenTheHandlerPanics(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	h := Idempotency(store, true, newMetrics(), slog.New(slog.DiscardHandler))(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic did not reach the outer middleware")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), idemRequest(t, ann, "/v1/x", "k1", `{}`))
	}()
	if len(store.records) != 0 || store.releases != 1 {
		t.Errorf("%d records left after %d releases, want none after 1", len(store.records), store.releases)
	}
}

func TestIdempotencyOutlivesTheClient(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	ctx, cancel := context.WithCancel(t.Context())
	h := Idempotency(store, true, newMetrics(), slog.New(slog.DiscardHandler))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			cancel() // the client hangs up while the request runs
			w.WriteHeader(http.StatusOK)
		}))

	h.ServeHTTP(httptest.NewRecorder(), idemRequest(t, ann, "/v1/x", "k1", `{}`).WithContext(
		principal.NewContext(ctx, ann)))

	if _, rec := store.only(t); !rec.Done {
		t.Errorf("record = %+v, want the response stored", rec)
	}
	if len(store.ctxErrs) != 1 || store.ctxErrs[0] != nil {
		t.Errorf("Complete ran with context errors %v, want a live context", store.ctxErrs)
	}
}

func TestIdempotencyWithoutAWorkingStore(t *testing.T) {
	t.Parallel()

	failing := newMemStore()
	failing.err = errors.New("redis down")
	undecodable := newMemStore()
	undecodable.records[ann.UserID.String()+":"+digest("POST /v1/x\nk1")] = memRecord{owner: "x", record: []byte("{")}

	for name, store := range map[string]IdempotencyStore{"failing store": failing, "no store": nil, "undecodable record": undecodable} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			next := &countingHandler{status: http.StatusCreated}
			h := Idempotency(store, true, newMetrics(), slog.New(slog.DiscardHandler))(next)
			for range 2 {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, idemRequest(t, ann, "/v1/x", "k1", `{}`))
				if rec.Code != http.StatusCreated || rec.Header().Get(IdempotentReplayedHeader) != "" {
					t.Fatalf("status = %d, replayed %q; want the handler's 201", rec.Code, rec.Header().Get(IdempotentReplayedHeader))
				}
			}
			if next.calls != 2 {
				t.Errorf("handler ran %d times, want 2: without a working store, nothing is replayed", next.calls)
			}

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, idemRequest(t, ann, "/v1/x", "", `{}`))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("request without a required key = %d, want 400 even without a store", rec.Code)
			}
		})
	}
}

func TestIdempotencyPassesUnusualRequestsThrough(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	next := &countingHandler{status: http.StatusOK}
	h := Idempotency(store, false, newMetrics(), slog.New(slog.DiscardHandler))(next)

	large := strings.Repeat("x", maxPeekBytes+1)
	for _, r := range []*http.Request{
		idemRequest(t, ann, "/v1/x", "", `{}`),                  // no key on an optional route
		idemRequest(t, domain.Principal{}, "/v1/x", "k1", `{}`), // no caller: the handler fails closed
		idemRequest(t, ann, "/v1/x", "k1", large),               // too large: the handler answers 413
	} {
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if next.calls != 3 || len(store.records) != 0 {
		t.Errorf("handler ran %d times, %d records stored; want 3 and none", next.calls, len(store.records))
	}
	if next.bodies[2] != large {
		t.Error("the handler did not get the whole large body")
	}
}

func TestIdempotencyLogsAResponseItCouldNotStore(t *testing.T) {
	t.Parallel()

	var buf strings.Builder
	store := newMemStore()
	store.stale = true
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	h := Idempotency(store, true, newMetrics(), logger)(&countingHandler{status: http.StatusOK})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, idemRequest(t, ann, "/v1/x", "k1", `{}`))

	if rec.Code != http.StatusOK || !strings.Contains(buf.String(), "outlived its key") {
		t.Errorf("status %d, log %q; want the response delivered and a warning", rec.Code, buf.String())
	}
}
