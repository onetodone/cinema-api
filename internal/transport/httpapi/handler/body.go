package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

// maxBodyBytes caps request bodies. Every request body of this API is a small JSON object.
const maxBodyBytes = 64 << 10

// decodeJSON reads the request body as one JSON object into dst. On failure it writes a problem and returns
// false:
//   - 415 UNSUPPORTED_MEDIA_TYPE unless the Content-Type is application/json;
//   - 413 BODY_TOO_LARGE above maxBodyBytes;
//   - 400 MALFORMED_BODY for invalid JSON, an unknown field, or data after the object;
//   - 400 VALIDATION_FAILED for a field of the wrong JSON type.
//
// Requiring application/json also keeps cross-site HTML forms out: a browser cannot send that type to
// another origin without a CORS preflight.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		problem.Write(w, r, problem.New(http.StatusUnsupportedMediaType, problem.CodeUnsupportedMediaType,
			"The request body must be JSON, sent with Content-Type: application/json."))
		return false
	}

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		problem.Write(w, r, bodyProblem(err))
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			problem.Write(w, r, bodyProblem(err))
		} else {
			problem.Write(w, r, malformedBody("The request body must contain a single JSON object."))
		}
		return false
	}
	return true
}

// bodyProblem explains why a request body could not be decoded.
func bodyProblem(err error) problem.Problem {
	var (
		tooLarge  *http.MaxBytesError
		syntaxErr *json.SyntaxError
		typeErr   *json.UnmarshalTypeError
	)
	switch {
	case errors.As(err, &tooLarge):
		return problem.New(http.StatusRequestEntityTooLarge, problem.CodeBodyTooLarge,
			fmt.Sprintf("The request body must not be larger than %d bytes.", tooLarge.Limit))
	case errors.Is(err, io.EOF):
		return malformedBody("The request body must not be empty.")
	case errors.As(err, &syntaxErr), errors.Is(err, io.ErrUnexpectedEOF):
		return malformedBody("The request body is not valid JSON.")
	case errors.As(err, &typeErr):
		if typeErr.Field == "" {
			return malformedBody("The request body must be a JSON object.")
		}
		return problem.Validation(problem.FieldError{Field: fieldPath(typeErr.Field), Message: "must be " + jsonKind(typeErr.Type)})
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		// encoding/json has no error type for this case, only this message.
		field := strings.TrimPrefix(err.Error(), "json: unknown field ")
		return malformedBody("The request body has an unknown field " + field + ".")
	default:
		return malformedBody("The request body could not be read.")
	}
}

// fieldPath turns a field path as encoding/json reports it, such as rows.0.seats, into the form the services use
// in validation errors, rows[0].seats, so that clients see one naming scheme.
func fieldPath(jsonPath string) string {
	parts := strings.Split(jsonPath, ".")
	var b strings.Builder
	for i, part := range parts {
		if _, err := strconv.Atoi(part); err == nil && i > 0 {
			b.WriteString("[" + part + "]")
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(part)
	}
	return b.String()
}

func malformedBody(detail string) problem.Problem {
	return problem.New(http.StatusBadRequest, problem.CodeMalformedBody, detail)
}

// jsonKind names the JSON type that decodes into t, for error messages.
func jsonKind(t reflect.Type) string {
	if t == nil {
		return "of another type"
	}
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "an integer"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice, reflect.Array:
		return "an array"
	case reflect.Map, reflect.Struct:
		return "an object"
	default:
		return "of another type"
	}
}
