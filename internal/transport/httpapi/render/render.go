// Package render writes HTTP responses.
package render

import (
	"encoding/json"
	"net/http"
)

// fallbackBody is sent when a response value cannot be encoded, so clients never receive a partial body.
const fallbackBody = `{"error":"internal server error"}` + "\n"

// JSON encodes v and writes it with the given status code.
// The value is encoded before any header is written, so an encoding failure still yields a clean 500.
func JSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(fallbackBody))
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}
