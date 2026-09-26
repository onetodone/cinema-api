package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/principal"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

// Headers of idempotent requests (draft-ietf-httpapi-idempotency-key-header).
const (
	IdempotencyKeyHeader = "Idempotency-Key"
	// IdempotentReplayedHeader marks a response that repeats the stored response of an earlier request.
	IdempotentReplayedHeader = "Idempotent-Replayed"
)

const (
	// IdempotencyTTL is how long the response to a request is replayed to requests with the same key.
	IdempotencyTTL = 24 * time.Hour
	// idempotencyLockTTL is how long a request counts as in progress for its key. It is longer than any request
	// takes, the provider call and settlement of a payment included, so a running request keeps its key. It only
	// runs out when the process died during the request; a retry then waits for at most this long.
	idempotencyLockTTL = time.Minute
	// maxIdempotencyKeyLength follows the Stripe and IETF draft convention.
	maxIdempotencyKeyLength = 255
	// maxStoredResponseBytes caps a stored response body. The responses of idempotent routes are a few KiB.
	maxStoredResponseBytes = 1 << 20
)

// replayedHeaders are the response headers that a replay repeats. The others describe the delivery, not the
// outcome.
var replayedHeaders = []string{"Content-Type", "Location"}

// IdempotencyStore keeps records that belong to an owner and expire. It is implemented by
// repository/redis.Idempotency.
type IdempotencyStore interface {
	// Claim stores record for owner under key, unless the key exists; it then returns the stored record.
	Claim(ctx context.Context, key, owner string, record []byte, ttl time.Duration) (existing []byte, claimed bool, err error)
	// Complete replaces the record under key if owner still owns it, and reports whether it did.
	Complete(ctx context.Context, key, owner string, record []byte, ttl time.Duration) (bool, error)
	// Release deletes the record under key if owner still owns it.
	Release(ctx context.Context, key, owner string) error
}

// idempotencyRecord is what the store keeps under a key: the fingerprint of the request, and once the request is
// done, its response.
type idempotencyRecord struct {
	Fingerprint string      `json:"fingerprint"` // SHA-256 of the request body
	Done        bool        `json:"done"`
	Status      int         `json:"status,omitempty"`
	Header      http.Header `json:"header,omitempty"`
	Body        []byte      `json:"body,omitempty"`
}

// Idempotency makes retries of a POST safe: a request with the Idempotency-Key header of an earlier request of the
// same caller, method, and path gets the earlier response again, with Idempotent-Replayed: true, instead of
// running a second time. It must run inside Authenticate, because keys belong to their caller.
//
//   - Without the header, the request runs normally, unless required is set: then it fails with 400
//     VALIDATION_FAILED. So does a key that is empty, too long, or not printable ASCII.
//   - While the first request with a key runs, a second one fails with 409 IDEMPOTENCY_IN_PROGRESS and
//     Retry-After: 1.
//   - A key reused with another request body fails with 422 IDEMPOTENCY_KEY_REUSED.
//   - Responses are stored for IdempotencyTTL, unless they report a transient problem: a 5xx, or anything with a
//     Retry-After, such as 409 SEAT_BUSY or 503 PAYMENT_PROVIDER_UNAVAILABLE. Those free the key, so that a retry
//     runs again.
//
// Without a store, or when the store fails, the header is still checked, and the request runs without the
// guarantee. It then relies on the database: the partial unique indexes allow one active booking per user and
// showtime and one payment in flight per booking, so a retry gets a conflict instead of a duplicate.
func Idempotency(store IdempotencyStore, required bool, m *metrics.Metrics, logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(IdempotencyKeyHeader)
			switch {
			case key == "" && !required:
				next.ServeHTTP(w, r)
				return
			case key == "":
				problem.Write(w, r, problem.Validation(problem.FieldError{Field: IdempotencyKeyHeader, Message: "is required"}))
				return
			case !validIdempotencyKey(key):
				problem.Write(w, r, problem.Validation(problem.FieldError{
					Field: IdempotencyKeyHeader, Message: "must be 1 to 255 printable ASCII characters",
				}))
				return
			}

			caller, ok := principal.FromContext(r.Context())
			body, readable := peekBody(r)
			if store == nil || !ok || !readable {
				// No caller: the handler fails closed. An unreadable body: the handler rejects it.
				next.ServeHTTP(w, r)
				return
			}

			g := idempotentRequest{
				store:       store,
				key:         caller.UserID.String() + ":" + digest(r.Method+" "+r.URL.Path+"\n"+key),
				owner:       uuid.NewV7().String(),
				fingerprint: digest(string(body)),
				metrics:     m,
				logger:      logger,
			}
			g.serve(w, r, next)
		})
	}
}

// idempotentRequest is one request with an Idempotency-Key, on its way through the middleware.
type idempotentRequest struct {
	store       IdempotencyStore
	key         string // the store key: caller, method, path, and header value
	owner       string // this request, as owner of the key while it runs
	fingerprint string
	metrics     *metrics.Metrics
	logger      *slog.Logger
}

func (g idempotentRequest) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	ctx := r.Context()
	pending, _ := json.Marshal(idempotencyRecord{Fingerprint: g.fingerprint})
	existing, claimed, err := g.store.Claim(ctx, g.key, g.owner, pending, idempotencyLockTTL)
	if err != nil {
		next.ServeHTTP(w, r) // fail open: the store recorded the failure
		return
	}
	if !claimed {
		g.answerFromRecord(w, r, next, existing)
		return
	}
	g.metrics.IdempotencyRequests.WithLabelValues(metrics.IdempotencyNew).Inc()

	// The outcome must be recorded even if the client hangs up, and the key freed even if the handler panics.
	after := context.WithoutCancel(ctx)
	completed := false
	defer func() {
		if !completed {
			if err := g.store.Release(after, g.key, g.owner); err != nil {
				g.logger.DebugContext(ctx, "idempotency key not released; it expires by itself", slog.Any("error", err))
			}
		}
	}()

	capture := &responseCapture{ResponseWriter: w, status: http.StatusOK}
	next.ServeHTTP(capture, r)
	if capture.status >= http.StatusInternalServerError || w.Header().Get("Retry-After") != "" || capture.overflow {
		return // transient or too large to keep: a retry runs again
	}

	rec := idempotencyRecord{
		Fingerprint: g.fingerprint, Done: true, Status: capture.status, Header: http.Header{}, Body: capture.body.Bytes(),
	}
	for _, h := range replayedHeaders {
		if v := w.Header().Values(h); len(v) > 0 {
			rec.Header[h] = v
		}
	}
	done, _ := json.Marshal(rec)
	stored, err := g.store.Complete(after, g.key, g.owner, done, IdempotencyTTL)
	switch {
	case err != nil:
		g.logger.DebugContext(ctx, "idempotent response not stored; the key expires by itself", slog.Any("error", err))
	case !stored:
		g.logger.WarnContext(ctx, "idempotent request outlived its key; its response was not stored",
			slog.Duration("key_ttl", idempotencyLockTTL))
	}
	completed = true
}

// answerFromRecord answers a request whose key another request already claimed.
func (g idempotentRequest) answerFromRecord(w http.ResponseWriter, r *http.Request, next http.Handler, raw []byte) {
	var rec idempotencyRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		g.logger.WarnContext(r.Context(), "undecodable idempotency record; running the request without it",
			slog.Any("error", err))
		next.ServeHTTP(w, r)
		return
	}

	switch {
	case rec.Fingerprint != g.fingerprint:
		g.metrics.IdempotencyRequests.WithLabelValues(metrics.IdempotencyKeyReused).Inc()
		problem.Write(w, r, problem.New(http.StatusUnprocessableEntity, problem.CodeIdempotencyKeyReused,
			"This Idempotency-Key was already used for a request with another body; use a new key for a new request."))
	case !rec.Done:
		g.metrics.IdempotencyRequests.WithLabelValues(metrics.IdempotencyInProgress).Inc()
		p := problem.New(http.StatusConflict, problem.CodeIdempotencyInProgress,
			"The first request with this Idempotency-Key is still running; try again in a moment.")
		p.RetryAfter = 1
		problem.Write(w, r, p)
	default:
		g.metrics.IdempotencyRequests.WithLabelValues(metrics.IdempotencyReplayed).Inc()
		for h, v := range rec.Header {
			w.Header()[h] = v
		}
		w.Header().Set(IdempotentReplayedHeader, "true")
		w.WriteHeader(rec.Status)
		_, _ = w.Write(rec.Body)
	}
}

// validIdempotencyKey accepts 1 to 255 printable ASCII characters. Keys are opaque; UUIDs are typical.
func validIdempotencyKey(key string) bool {
	if key == "" || len(key) > maxIdempotencyKeyLength {
		return false
	}
	for i := range len(key) {
		if key[i] < 0x20 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// responseCapture passes a response through and keeps a copy of its status and body.
type responseCapture struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	body        bytes.Buffer
	overflow    bool // the body exceeded maxStoredResponseBytes and was not kept
}

func (c *responseCapture) WriteHeader(code int) {
	if !c.wroteHeader && code >= http.StatusOK { // 1xx responses are informational
		c.status = code
		c.wroteHeader = true
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *responseCapture) Write(b []byte) (int, error) {
	c.wroteHeader = true
	if !c.overflow {
		if c.body.Len()+len(b) > maxStoredResponseBytes {
			c.overflow = true
			c.body.Reset()
		} else {
			c.body.Write(b)
		}
	}
	return c.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (c *responseCapture) Unwrap() http.ResponseWriter {
	return c.ResponseWriter
}
