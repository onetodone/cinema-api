package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

type sampleBody struct {
	Email string  `json:"email"`
	Count int     `json:"count"`
	IDs   []int64 `json:"ids"`
}

// serveBody runs decodeJSON on a request with the given content type and body. It answers 204 on success.
func serveBody(t *testing.T, contentType, body string) (*httptest.ResponseRecorder, sampleBody) {
	t.Helper()
	var got sampleBody
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if decodeJSON(w, r, &got) {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/x", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, got
}

func TestDecodeJSONAcceptsAJSONObject(t *testing.T) {
	t.Parallel()

	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON"} {
		rec, got := serveBody(t, ct, `{"email":"ann@example.com","count":2,"ids":[1,2]}`+"\n")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: status = %d, body %s", ct, rec.Code, rec.Body.String())
		}
		if got.Email != "ann@example.com" || got.Count != 2 || len(got.IDs) != 2 {
			t.Errorf("%s: decoded %+v", ct, got)
		}
	}
}

func TestDecodeJSONRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contentType string
		body        string
		status      int
		code        string
		detail      string
		field       string
	}{
		{
			name: "no content type", body: `{}`,
			status: http.StatusUnsupportedMediaType, code: problem.CodeUnsupportedMediaType,
		},
		{
			name: "form content type", contentType: "application/x-www-form-urlencoded", body: `email=a`,
			status: http.StatusUnsupportedMediaType, code: problem.CodeUnsupportedMediaType,
		},
		{
			name: "empty body", contentType: "application/json", body: ``,
			status: http.StatusBadRequest, code: problem.CodeMalformedBody, detail: "The request body must not be empty.",
		},
		{
			name: "invalid json", contentType: "application/json", body: `{email: 1}`,
			status: http.StatusBadRequest, code: problem.CodeMalformedBody, detail: "The request body is not valid JSON.",
		},
		{
			name: "truncated json", contentType: "application/json", body: `{"email":`,
			status: http.StatusBadRequest, code: problem.CodeMalformedBody, detail: "The request body is not valid JSON.",
		},
		{
			name: "array instead of object", contentType: "application/json", body: `[1]`,
			status: http.StatusBadRequest, code: problem.CodeMalformedBody, detail: "The request body must be a JSON object.",
		},
		{
			name: "unknown field", contentType: "application/json", body: `{"email":"a","role":"admin"}`,
			status: http.StatusBadRequest, code: problem.CodeMalformedBody,
			detail: `The request body has an unknown field "role".`,
		},
		{
			name: "two objects", contentType: "application/json", body: `{} {}`,
			status: http.StatusBadRequest, code: problem.CodeMalformedBody,
			detail: "The request body must contain a single JSON object.",
		},
		{
			name: "garbage after the object", contentType: "application/json", body: `{"count":1} x`,
			status: http.StatusBadRequest, code: problem.CodeMalformedBody,
			detail: "The request body must contain a single JSON object.",
		},
		{
			name: "string for an integer", contentType: "application/json", body: `{"count":"two"}`,
			status: http.StatusBadRequest, code: problem.CodeValidationFailed, field: "count",
		},
		{
			name: "fraction for an integer", contentType: "application/json", body: `{"count":1.5}`,
			status: http.StatusBadRequest, code: problem.CodeValidationFailed, field: "count",
		},
		{
			name: "number for a string", contentType: "application/json", body: `{"email":42}`,
			status: http.StatusBadRequest, code: problem.CodeValidationFailed, field: "email",
		},
		{
			name: "object for an array", contentType: "application/json", body: `{"ids":{}}`,
			status: http.StatusBadRequest, code: problem.CodeValidationFailed, field: "ids",
		},
		{
			name: "too large", contentType: "application/json",
			body:   `{"email":"` + strings.Repeat("a", maxBodyBytes) + `"}`,
			status: http.StatusRequestEntityTooLarge, code: problem.CodeBodyTooLarge,
		},
		{
			name: "too large after a valid object", contentType: "application/json",
			body:   `{"count":1}` + strings.Repeat(" ", maxBodyBytes),
			status: http.StatusRequestEntityTooLarge, code: problem.CodeBodyTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec, _ := serveBody(t, tt.contentType, tt.body)
			p := assertProblem(t, rec, tt.status, tt.code)
			if tt.detail != "" && p.Detail != tt.detail {
				t.Errorf("detail = %q, want %q", p.Detail, tt.detail)
			}
			if tt.field != "" && (len(p.Errors) != 1 || p.Errors[0].Field != tt.field) {
				t.Errorf("errors = %+v, want one error for field %q", p.Errors, tt.field)
			}
		})
	}
}

func TestDecodeJSONNamesTheExpectedType(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		`{"count":"two"}`: "must be an integer",
		`{"email":true}`:  "must be a string",
		`{"ids":"1,2"}`:   "must be an array",
		`{"ids":["a"]}`:   "must be an integer",
	}
	for body, want := range tests {
		rec, _ := serveBody(t, "application/json", body)
		p := assertProblem(t, rec, http.StatusBadRequest, problem.CodeValidationFailed)
		if len(p.Errors) != 1 || p.Errors[0].Message != want {
			t.Errorf("%s: errors = %+v, want %q", body, p.Errors, want)
		}
	}
}
