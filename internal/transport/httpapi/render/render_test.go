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

func TestValidatedJSON(t *testing.T) {
	t.Parallel()

	serve := func(ifNoneMatch ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		for _, v := range ifNoneMatch {
			req.Header.Add("If-None-Match", v)
		}
		rec := httptest.NewRecorder()
		ValidatedJSON(rec, req, map[string]int{"held": 3})
		return rec
	}

	rec := serve()
	// The SHA-256 of the body, which ends with a newline like every JSON answer, in unpadded base64url:
	// printf '{"held":3}\n' | openssl dgst -sha256 -binary | base64 | tr '+/' '-_' | tr -d =
	const etag = `W/"_xiyV6uCKMp65F0a5RmocZLNa8hASDD2_Y5JERWfJ8Y"`
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"held\":3}\n" {
		t.Fatalf("answer = %d %q", rec.Code, rec.Body.String())
	}
	for k, want := range map[string]string{"ETag": etag, "Cache-Control": "no-cache", "Content-Type": JSONContentType} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	if rec := serve(etag); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Errorf("matching If-None-Match = %d, body %q, Content-Type %q; want a bare 304",
			rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
	} else if rec.Header().Get("ETag") != etag || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("304 headers = %v", rec.Header())
	}
	if rec := serve(`"other"`); rec.Code != http.StatusOK {
		t.Errorf("another ETag = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	ValidatedJSON(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil), map[string]any{"bad": make(chan int)})
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("ETag") != "" {
		t.Errorf("unencodable value = %d, ETag %q; want 500 without one", rec.Code, rec.Header().Get("ETag"))
	}
}

func TestNoneMatch(t *testing.T) {
	t.Parallel()

	const etag = `W/"abc"`
	tests := []struct {
		lines []string
		want  bool
	}{
		{nil, false},
		{[]string{`W/"abc"`}, true},
		{[]string{`"abc"`}, true}, // weak comparison: the strong form matches the weak tag
		{[]string{`"ab"`}, false},
		{[]string{`"abcd"`}, false},
		{[]string{`abc`}, false}, // not quoted
		{[]string{`*`}, true},
		{[]string{`"x", W/"y" ,"abc"`}, true},
		{[]string{`"x"`, `W/"abc"`}, true}, // several field lines
		{[]string{`"a,b", "abc"`}, true},   // a comma inside a tag
		{[]string{`"x", junk, "abc"`}, false},
		{[]string{`"x", "abc`}, false}, // unterminated
		{[]string{`W/`}, false},
		{[]string{`W/"abc"junk`}, true},
		{[]string{``, ` , `}, false},
	}
	for _, tt := range tests {
		if got := noneMatch(tt.lines, etag); got != tt.want {
			t.Errorf("noneMatch(%q) = %v, want %v", tt.lines, got, tt.want)
		}
	}
}
