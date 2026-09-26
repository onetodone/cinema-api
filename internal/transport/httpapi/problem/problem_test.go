package problem

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

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
