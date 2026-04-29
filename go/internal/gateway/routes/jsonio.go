package routes

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// errMaxBytes is what http.MaxBytesReader returns when the body exceeds the
// configured cap. We want to map it to 413 (Payload Too Large) at decode time.
type maxBytesError interface {
	error
	MaxBytesError()
}

const defaultMaxBodyBytes = 1 << 20 // 1 MiB

// writeJSON marshals v into the response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a uniform `{"detail": "<msg>"}` body. Uses "detail" to
// match FastAPI's default error envelope so cross-language clients (and the
// existing Python tests' assertions) keep working.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"detail": msg})
}

// decodeJSON reads + json-decodes the request body into out. Returns a status
// code + message on failure (422 for malformed/missing JSON, mirroring
// FastAPI's pydantic validator). The caller must NOT have written anything to
// w yet.
func decodeJSON(r *http.Request, out any) (int, string, error) {
	if r.Body == nil {
		return http.StatusUnprocessableEntity, "missing request body", errors.New("nil body")
	}
	r.Body = http.MaxBytesReader(nil, r.Body, defaultMaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		var mb maxBytesError
		if errors.As(err, &mb) {
			return http.StatusRequestEntityTooLarge, "request body too large", err
		}
		if errors.Is(err, io.EOF) {
			return http.StatusUnprocessableEntity, "request body is empty", err
		}
		// Differentiate strict-mode "unknown field" from other JSON syntax
		// errors so callers can see why a field was rejected. FastAPI returns
		// 422 in either case; preserve that.
		return http.StatusUnprocessableEntity, "malformed JSON: " + err.Error(), err
	}
	// Defend against trailing junk: a second token in the same body indicates
	// the client sent multiple JSON values, which is not a valid request.
	if dec.More() {
		return http.StatusUnprocessableEntity, "request body has trailing data", errors.New("trailing data")
	}
	return 0, "", nil
}

// nonEmpty returns true iff s is non-empty after trimming surrounding spaces.
// Used in the lightweight pydantic-equivalent validators on each handler.
func nonEmpty(s string) bool { return strings.TrimSpace(s) != "" }
