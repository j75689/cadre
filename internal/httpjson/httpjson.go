// Package httpjson holds the JSON conventions shared by Cadre's HTTP APIs:
// request decoding, the error envelope, and which error gets which status.
package httpjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// Error codes. Each API documents which ones it returns.
const (
	CodeInvalid           = "invalid"            // 400: the request is malformed
	CodeNotFound          = "not_found"          // 404: the object does not exist
	CodeAlreadyExists     = "already_exists"     // 409: the object exists already
	CodeRunMismatch       = "run_mismatch"       // 409: the run lost its task; stop
	CodeStepMismatch      = "step_mismatch"      // 409: step mode; drop this result
	CodeAttemptMismatch   = "attempt_mismatch"   // 409: step mode; the attempt is over, stop
	CodeStepTaken         = "step_taken"         // 409: step mode; another worker holds the step
	CodeStepExhausted     = "step_exhausted"     // 409: step mode; no worker may take the step again
	CodeInvalidTransition = "invalid_transition" // 409: not allowed from the task's phase
	CodeInProgress        = "in_progress"        // 409: being done by another request
	CodeEntryPending      = "entry_pending"      // 409: the entry waited on is not in the thread yet
	CodeConflict          = "conflict"           // 409: the object changed since it was read; read it again
	CodeUnauthenticated   = "unauthenticated"    // 401: a valid credential is required
	CodeForbidden         = "forbidden"          // 403: not this caller's to do
	CodeNoRole            = "no_role"            // 403: the caller's role on the team does not allow it
	CodeUnavailable       = "unavailable"        // 503: retry later
	CodeServerForbidden   = "server_forbidden"   // 500: cadre-server's own RBAC refuses it; fix its install
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
	return DecodeLimit(w, r, v, MaxBody)
}

// DecodeLimit is Decode for a body of up to limit bytes.
func DecodeLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	err := dec.Decode(v)
	if err == nil && dec.More() {
		err = errors.New("more than one JSON value")
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		WriteError(w, http.StatusRequestEntityTooLarge, CodeInvalid, fmt.Sprintf("request body over %d bytes", limit))
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

// WriteVerbatim is Write without escaping HTML characters, for a body that
// carries stored JSON back exactly as it was stored.
func WriteVerbatim(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// WriteError writes the error envelope.
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	Write(w, status, ErrorResponse{Error: ErrorBody{Code: code, Message: msg}})
}

// Classify says which error code err maps to. It is the one classification
// every Cadre API uses, the HTTP APIs through Fail and the MCP tools through
// their tool errors, so an error cannot be a client mistake on one surface
// and a retriable fault on another.
func Classify(err error) string {
	switch {
	case errors.Is(err, task.ErrRunMismatch):
		return CodeRunMismatch
	case errors.Is(err, task.ErrStepMismatch):
		return CodeStepMismatch
	case errors.Is(err, task.ErrAttemptMismatch):
		return CodeAttemptMismatch
	case errors.Is(err, task.ErrStepTaken):
		return CodeStepTaken
	case errors.Is(err, task.ErrStepExhausted):
		return CodeStepExhausted
	case errors.Is(err, task.ErrInvalidTransition):
		return CodeInvalidTransition
	case errors.Is(err, manager.ErrInProgress):
		return CodeInProgress
	case errors.Is(err, manager.ErrEntryPending):
		return CodeEntryPending
	case errors.Is(err, manager.ErrTeamChanged), errors.Is(err, manager.ErrAmbiguous):
		return CodeConflict
	case errors.Is(err, task.ErrForbidden):
		return CodeForbidden
	case errors.Is(err, manager.ErrInvalid), errors.Is(err, task.ErrInvalidEvent),
		errors.Is(err, storage.ErrInvalidQuery), errors.Is(err, storage.ErrInvalid), errors.Is(err, storage.ErrInvalidName):
		return CodeInvalid
	case errors.Is(err, storage.ErrNotFound):
		return CodeNotFound
	case errors.Is(err, storage.ErrAlreadyExists):
		return CodeAlreadyExists
	}
	return CodeUnavailable
}

// Fail answers err with the status and code Classify gives it. Only
// validation errors, which describe the request itself, are sent in full;
// other errors are answered with fixed text, and the unexpected ones are
// logged, so storage details never reach the client.
func Fail(w http.ResponseWriter, log *slog.Logger, err error) {
	switch code := Classify(err); code {
	case CodeRunMismatch:
		WriteError(w, http.StatusConflict, code, task.ErrRunMismatch.Error())
	case CodeStepMismatch:
		WriteError(w, http.StatusConflict, code, task.ErrStepMismatch.Error())
	case CodeAttemptMismatch:
		WriteError(w, http.StatusConflict, code, task.ErrAttemptMismatch.Error())
	case CodeStepTaken:
		WriteError(w, http.StatusConflict, code, task.ErrStepTaken.Error())
	case CodeStepExhausted:
		WriteError(w, http.StatusConflict, code, task.ErrStepExhausted.Error())
	case CodeInvalidTransition:
		WriteError(w, http.StatusConflict, code, task.ErrInvalidTransition.Error())
	case CodeInvalid:
		WriteError(w, http.StatusBadRequest, code, err.Error())
	case CodeEntryPending:
		WriteError(w, http.StatusConflict, code, manager.ErrEntryPending.Error())
	case CodeConflict:
		msg := manager.ErrTeamChanged.Error()
		if a := (*manager.AmbiguousNumber)(nil); errors.As(err, &a) {
			msg = a.Error() // names the tasks, which the caller may see
		}
		WriteError(w, http.StatusConflict, code, msg)
	case CodeForbidden:
		WriteError(w, http.StatusForbidden, code, task.ErrForbidden.Error())
	case CodeInProgress:
		// The message says when to come back, and nothing else.
		msg := manager.ErrInProgress.Error()
		if p := (*manager.CallInProgress)(nil); errors.As(err, &p) {
			msg = p.Error()
		}
		WriteError(w, http.StatusConflict, code, msg)
	case CodeNotFound:
		WriteError(w, http.StatusNotFound, code, "not found")
	case CodeAlreadyExists:
		WriteError(w, http.StatusConflict, code, "already exists")
	default:
		if errors.Is(err, storage.ErrStreamCollision) {
			// A server-side layout fault: retriable for the client,
			// and logged so it is investigated.
			log.Error("log layout fault", "err", err)
		} else {
			// Busy objects and storage failures are worth retrying.
			log.Warn("request failed", "err", err)
		}
		WriteError(w, http.StatusServiceUnavailable, CodeUnavailable, "temporarily unavailable; retry")
	}
}

// LogRequests logs every request's method, path, status and duration;
// health probes at debug level, since they arrive every few seconds.
// Credentials never travel in paths (the per-run MCP token is a header), so
// paths are logged as they are.
func LogRequests(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		level := slog.LevelInfo
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			level = slog.LevelDebug
		}
		log.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer, for
// flushing streamed (MCP) responses and the rest.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
