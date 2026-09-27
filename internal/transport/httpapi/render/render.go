// Package render writes HTTP responses.
package render

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// JSONContentType is the media type of regular JSON responses.
const JSONContentType = "application/json; charset=utf-8"

// fallbackBody is sent when a response value cannot be encoded, so clients never receive a partial body.
const fallbackBody = `{"error":"internal server error"}` + "\n"

// JSON encodes v and writes it as application/json with the given status code.
func JSON(w http.ResponseWriter, status int, v any) {
	Encode(w, status, JSONContentType, v)
}

// Encode encodes v as JSON and writes it with the given status code and content type.
// The value is encoded before any header is written, so an encoding failure still yields a clean 500.
func Encode(w http.ResponseWriter, status int, contentType string, v any) {
	body, ok := marshal(w, v)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// ValidatedJSON writes v as a 200 JSON answer that clients can poll cheaply (RFC 9110 §8.8.3, §13.1.2). The answer
// carries a weak ETag computed from its body and Cache-Control: no-cache, so browsers and caches keep it but ask
// again every time. A request whose If-None-Match names the current tag gets 304 Not Modified without a body.
//
// The tag names the body, so two replicas, or a cached and a freshly read seat map, give the same tag for the same
// content, and nothing has to be stored for it. It is weak because it promises the same content, not the same
// bytes on the wire: a proxy that compresses the answer keeps it.
func ValidatedJSON(w http.ResponseWriter, r *http.Request, v any) {
	body, ok := marshal(w, v)
	if !ok {
		return
	}
	sum := sha256.Sum256(body)
	etag := `W/"` + base64.RawURLEncoding.EncodeToString(sum[:]) + `"`

	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if noneMatch(r.Header.Values("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", JSONContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// marshal encodes v with a trailing newline. On failure it writes the fallback 500 and returns false.
func marshal(w http.ResponseWriter, v any) ([]byte, bool) {
	body, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", JSONContentType)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(fallbackBody))
		return nil, false
	}
	return append(body, '\n'), true
}

// noneMatch reports whether the If-None-Match field lines name etag, or are "*", by the weak comparison that
// RFC 9110 §13.1.2 prescribes: W/"x" and "x" match each other. Parsing stops at the first malformed entry, and
// the entries before it still count.
func noneMatch(lines []string, etag string) bool {
	opaque := strings.TrimPrefix(etag, "W/")
	for _, line := range lines {
		for {
			line = strings.TrimLeft(line, " \t,")
			if line == "" {
				break
			}
			if line[0] == '*' {
				return true
			}
			line = strings.TrimPrefix(line, "W/")
			if line == "" || line[0] != '"' {
				break
			}
			end := strings.IndexByte(line[1:], '"')
			if end < 0 {
				break
			}
			if line[:end+2] == opaque {
				return true
			}
			line = line[end+2:]
		}
	}
	return false
}
