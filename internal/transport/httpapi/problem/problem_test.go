package problem

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/requestid"
)

func TestFromErrorMapsDomainKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		err    error
		status int
		code   string
		detail string
	}{
		{
			name:   "not found",
			err:    domain.NotFound(domain.CodeMovieNotFound, "movie 7 not found"),
			status: http.StatusNotFound,
			code:   domain.CodeMovieNotFound,
			detail: "movie 7 not found",
		},
		{
			name:   "conflict wrapped by a repository",
			err:    fmt.Errorf("create showtime: %w", domain.Conflict(domain.CodeHallOverlap, "overlap")),
			status: http.StatusConflict,
			code:   domain.CodeHallOverlap,
			detail: "overlap",
		},
		{
			name:   "invalid",
			err:    domain.Invalid("TOO_MANY_SEATS", "at most 10 seats"),
			status: http.StatusUnprocessableEntity,
			code:   "TOO_MANY_SEATS",
			detail: "at most 10 seats",
		},
		{
			name:   "unauthenticated",
			err:    domain.Unauthenticated(domain.CodeInvalidCredentials, "wrong email or password"),
			status: http.StatusUnauthorized,
			code:   domain.CodeInvalidCredentials,
			detail: "wrong email or password",
		},
		{
			name:   "forbidden",
			err:    domain.Forbidden(domain.CodeForbidden, "admins only"),
			status: http.StatusForbidden,
			code:   domain.CodeForbidden,
			detail: "admins only",
		},
		{
			name:   "gone",
			err:    domain.Gone(domain.CodeBookingExpired, "the hold has run out"),
			status: http.StatusGone,
			code:   domain.CodeBookingExpired,
			detail: "the hold has run out",
		},
		{
			name:   "unknown error is hidden",
			err:    errors.New("pq: password authentication failed for user postgres"),
			status: http.StatusInternalServerError,
			code:   CodeInternal,
			detail: "An unexpected error occurred.",
		},
		{
			name:   "domain error with unknown kind",
			err:    &domain.Error{Kind: errors.New("odd"), Code: "ODD", Message: "odd"},
			status: http.StatusInternalServerError,
			code:   CodeInternal,
			detail: "An unexpected error occurred.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := FromError(tt.err)
			if p.Status != tt.status || p.Code != tt.code || p.Detail != tt.detail {
				t.Errorf("got %d %s %q, want %d %s %q", p.Status, p.Code, p.Detail, tt.status, tt.code, tt.detail)
			}
			if p.Type != "about:blank" || p.Title != http.StatusText(tt.status) {
				t.Errorf("type/title = %q/%q", p.Type, p.Title)
			}
		})
	}
}

func TestFromErrorListsUnavailableSeats(t *testing.T) {
	t.Parallel()

	p := FromError(fmt.Errorf("create booking: %w", domain.SeatsUnavailable([]int64{7, 9})))
	if p.Status != http.StatusConflict || p.Code != domain.CodeSeatUnavailable || p.Detail != "seats 7, 9 are already held or sold" {
		t.Errorf("got %d %s %q", p.Status, p.Code, p.Detail)
	}
	if len(p.UnavailableSeatIDs) != 2 || p.UnavailableSeatIDs[0] != 7 || p.UnavailableSeatIDs[1] != 9 {
		t.Errorf("unavailable seats = %v, want [7 9]", p.UnavailableSeatIDs)
	}

	other := FromError(domain.Conflict(domain.CodeBookingNotCancelable, "paid"))
	if other.UnavailableSeatIDs != nil || other.BookingID != "" || other.RetryAfter != 0 {
		t.Errorf("a plain conflict got extensions: %+v", other)
	}
}

func TestFromErrorNamesTheActiveBooking(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("01920000-0000-7000-8000-0000000000b1")
	p := FromError(fmt.Errorf("create booking: %w", domain.ActiveBookingExists(11, id)))
	if p.Status != http.StatusConflict || p.Code != domain.CodeActiveBookingExists || p.RetryAfter != 0 {
		t.Errorf("got %d %s, Retry-After %d", p.Status, p.Code, p.RetryAfter)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"booking_id":"01920000-0000-7000-8000-0000000000b1"`) {
		t.Errorf("JSON = %s, want booking_id", raw)
	}

	// A booking that ended before it could be read leaves the member out.
	raw, err = json.Marshal(FromError(domain.ActiveBookingExists(11, uuid.UUID{})))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "booking_id") {
		t.Errorf("JSON = %s, want no booking_id", raw)
	}
}

func TestBusyErrorsAskForARetry(t *testing.T) {
	t.Parallel()

	p := FromError(domain.Busy(domain.CodeSeatBusy, "locked"))
	if p.Status != http.StatusConflict || p.Code != domain.CodeSeatBusy || p.RetryAfter != 1 {
		t.Fatalf("got %+v, want 409 SEAT_BUSY with a retry after 1 s", p)
	}

	rec := httptest.NewRecorder()
	Write(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/bookings", nil), p)
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, leaked := body["RetryAfter"]; leaked {
		t.Error("RetryAfter is part of the body")
	}
}

func TestPaymentErrors(t *testing.T) {
	t.Parallel()

	p := FromError(fmt.Errorf("pay: %w", domain.PaymentDeclined("insufficient_funds")))
	if p.Status != http.StatusPaymentRequired || p.Code != domain.CodePaymentDeclined || p.DeclineCode != "insufficient_funds" {
		t.Errorf("declined = %+v, want 402 PAYMENT_DECLINED with the decline code", p)
	}

	p = FromError(domain.Unavailable(domain.CodePaymentProviderUnavailable, "try later"))
	if p.Status != http.StatusServiceUnavailable || p.Code != domain.CodePaymentProviderUnavailable || p.RetryAfter != 5 {
		t.Errorf("unavailable = %+v, want 503 with a retry after 5 s", p)
	}
	if p.DeclineCode != "" {
		t.Errorf("a non-decline got a decline code: %+v", p)
	}
}

func TestWriteFillsInstanceAndRequestID(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/movies/7?x=1", nil)
	req = req.WithContext(requestid.NewContext(req.Context(), "req-7"))
	rec := httptest.NewRecorder()

	Write(rec, req, Validation(FieldError{Field: "limit", Message: "must be between 1 and 100"}))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != ContentType {
		t.Errorf("Content-Type = %q, want %q", got, ContentType)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"type":       "about:blank",
		"title":      "Bad Request",
		"status":     400.0,
		"code":       CodeValidationFailed,
		"instance":   "/v1/movies/7",
		"request_id": "req-7",
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %v, want %v", k, body[k], v)
		}
	}
	errs, _ := body["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want one field error", body["errors"])
	}
}

func TestWriteKeepsExplicitInstance(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	p := New(http.StatusNotFound, CodeNotFound, "")
	p.Instance = "/custom"
	Write(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/other", nil), p)

	var body Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Instance != "/custom" {
		t.Errorf("instance = %q, want /custom", body.Instance)
	}
	if body.RequestID != "" {
		t.Errorf("request_id = %q, want it omitted without the middleware", body.RequestID)
	}
}

func TestFromErrorListsValidationFields(t *testing.T) {
	t.Parallel()

	var v domain.Violations
	v.Add("email", "is required")
	v.Add("password", "must be at least %d characters", 8)

	p := FromError(fmt.Errorf("register: %w", v.Err()))
	if p.Status != http.StatusBadRequest || p.Code != CodeValidationFailed {
		t.Fatalf("got %d %s, want 400 %s", p.Status, p.Code, CodeValidationFailed)
	}
	want := []FieldError{
		{Field: "email", Message: "is required"},
		{Field: "password", Message: "must be at least 8 characters"},
	}
	if fmt.Sprint(p.Errors) != fmt.Sprint(want) {
		t.Errorf("errors = %v, want %v", p.Errors, want)
	}
}

func TestWriteAddsBearerChallengeTo401(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/auth/login", nil)

	rec := httptest.NewRecorder()
	Write(rec, req, New(http.StatusUnauthorized, domain.CodeInvalidCredentials, ""))
	if got := rec.Header().Get("WWW-Authenticate"); got != BearerChallenge {
		t.Errorf("WWW-Authenticate = %q, want %q", got, BearerChallenge)
	}

	rec = httptest.NewRecorder()
	rec.Header().Set("WWW-Authenticate", BearerChallenge+`, error="invalid_token"`)
	Write(rec, req, New(http.StatusUnauthorized, domain.CodeInvalidToken, ""))
	if got := rec.Header().Get("WWW-Authenticate"); got != BearerChallenge+`, error="invalid_token"` {
		t.Errorf("a specific challenge was replaced: WWW-Authenticate = %q", got)
	}

	rec = httptest.NewRecorder()
	Write(rec, req, New(http.StatusForbidden, domain.CodeForbidden, ""))
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("a 403 got WWW-Authenticate %q", got)
	}
}

func TestTooManyRequestsRoundsRetryAfterUp(t *testing.T) {
	t.Parallel()

	for retryAfter, want := range map[time.Duration]int{
		0:                       1,
		time.Millisecond:        1,
		time.Second:             1,
		1001 * time.Millisecond: 2,
		59*time.Second + 1:      60,
	} {
		p := TooManyRequests(retryAfter)
		if p.Status != http.StatusTooManyRequests || p.Code != CodeRateLimited || p.RetryAfter != want {
			t.Errorf("TooManyRequests(%s) = %d %s, Retry-After %d; want 429 %s, %d",
				retryAfter, p.Status, p.Code, p.RetryAfter, CodeRateLimited, want)
		}
	}
}
