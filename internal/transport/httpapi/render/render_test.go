package render

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJSONWritesStatusHeaderAndBody(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	JSON(rec, http.StatusCreated, map[string]string{"status": "ok"})

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Body.String(); got != "{\"status\":\"ok\"}\n" {
		t.Errorf("body = %q", got)
	}
}

func TestJSONFallsBackOnEncodingError(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	JSON(rec, http.StatusOK, map[string]any{"bad": make(chan int)})

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if got := rec.Body.String(); got != fallbackBody {
		t.Errorf("body = %q, want %q", got, fallbackBody)
	}
}
