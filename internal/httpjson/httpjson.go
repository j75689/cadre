// Package httpjson holds the JSON conventions shared by Cadre's HTTP APIs:
// request decoding and the error envelope.
package httpjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Error codes. Each API documents which ones it returns.
const (
	CodeInvalid           = "invalid"            // 400: the request is malformed
	CodeNotFound          = "not_found"          // 404: the object does not exist
	CodeAlreadyExists     = "already_exists"     // 409: the object exists already
	CodeRunMismatch       = "run_mismatch"       // 409: the run lost its task; stop
	CodeStepMismatch      = "step_mismatch"      // 409: step mode; drop this result
	CodeInvalidTransition = "invalid_transition" // 409: not allowed from the task's phase
	CodeUnavailable       = "unavailable"        // 503: retry later
)

// ErrorResponse is the body of every error.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody describes an error.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// MaxBody caps a request body; API requests are small.
const MaxBody = 1 << 20

// Decode reads one JSON value from the request body into v. Unknown fields
// are ignored, so an API can grow by adding fields without breaking older
// servers. On failure it writes 413 for an oversized body or 400 otherwise,
// and returns false.
func Decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBody))
	err := dec.Decode(v)
	if err == nil && dec.More() {
		err = errors.New("more than one JSON value")
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		WriteError(w, http.StatusRequestEntityTooLarge, CodeInvalid, fmt.Sprintf("request body over %d bytes", MaxBody))
		return false
	case err != nil:
		WriteError(w, http.StatusBadRequest, CodeInvalid, fmt.Sprintf("decode request: %v", err))
		return false
	}
	return true
}

// Write writes v as a JSON response.
func Write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the error envelope.
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	Write(w, status, ErrorResponse{Error: ErrorBody{Code: code, Message: msg}})
}
