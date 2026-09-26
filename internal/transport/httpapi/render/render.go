// Package render writes HTTP responses.
package render

import (
	"encoding/json"
	"net/http"
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
	body, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", JSONContentType)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(fallbackBody))
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}
