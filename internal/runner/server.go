package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// maxBody caps a request body; protocol requests are small.
const maxBody = 1 << 20

// Server serves /runner/v1 on a Task Manager. In M0 it is unauthenticated.
type Server struct {
	m   *manager.Manager
	log *slog.Logger
}

// NewServer returns the protocol server.
func NewServer(m *manager.Manager, log *slog.Logger) *Server { return &Server{m: m, log: log} }

// Register adds the protocol's routes to mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+PathClaim, s.claim)
	mux.HandleFunc("POST "+PathHeartbeat, s.heartbeat)
	mux.HandleFunc("POST "+PathEvents, s.events)
	mux.HandleFunc("POST "+PathFinish, s.finish)
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	var req ClaimRequest
	if !decode(w, r, &req) {
		return
	}
	c, err := s.m.Claim(r.Context(), manager.ClaimRequest{Runner: req.Runner, Namespaces: req.Namespaces, Labels: req.Labels})
	if err != nil {
		s.fail(w, err)
		return
	}
	if c == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	cfg := s.m.Config()
	t := c.Task
	writeJSON(w, http.StatusOK, ClaimResponse{
		Task: ClaimedTask{
			Namespace: t.Namespace, Name: t.Name, Number: t.Status.Number,
			Title: t.Spec.Title, Description: t.Spec.Description, Assignee: t.Spec.Assignee,
			Priority: t.Spec.Priority, ParentRef: t.Spec.ParentRef, Labels: t.Spec.Labels,
		},
		RunID:            c.RunID,
		Mode:             c.Mode,
		LeaseUntil:       c.LeaseUntil,
		LeaseSeconds:     int(cfg.Lease.Seconds()),
		HeartbeatSeconds: int(cfg.Heartbeat.Seconds()),
	})
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	var req HeartbeatRequest
	if !decode(w, r, &req) || !validRef(w, req.Task, req.RunID) {
		return
	}
	until, err := s.m.Heartbeat(r.Context(), key(req.Task), req.RunID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, HeartbeatResponse{LeaseUntil: until, LeaseSeconds: int(s.m.Config().Lease.Seconds())})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	var req EventsRequest
	if !decode(w, r, &req) || !validRef(w, req.Task, req.RunID) {
		return
	}
	err := s.m.RecordEvent(r.Context(), key(req.Task), manager.Event{
		RunID: req.RunID, Seq: req.Seq,
		InputTokens: req.Usage.InputTokens, OutputTokens: req.Usage.OutputTokens,
		ContextTokens: req.ContextTokens, Progress: req.Progress,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) finish(w http.ResponseWriter, r *http.Request) {
	var req FinishRequest
	if !decode(w, r, &req) || !validRef(w, req.Task, req.RunID) {
		return
	}
	if (req.Outcome == v1alpha1.RunOutcomeWaiting) != (req.Waiting != nil) {
		writeError(w, http.StatusBadRequest, CodeInvalid, "waiting is required exactly when the outcome is Waiting")
		return
	}
	mreq := manager.FinishRequest{RunID: req.RunID, Step: req.Step, Outcome: req.Outcome}
	if req.Waiting != nil {
		mreq.Reason, mreq.ThreadSeq = req.Waiting.Reason, req.Waiting.ThreadSeq
	}
	for _, st := range req.Subtasks {
		mreq.Subtasks = append(mreq.Subtasks, manager.Subtask{ID: st.ID, Spec: st.Spec})
	}
	phase, again, err := s.m.Finish(r.Context(), key(req.Task), mreq)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, FinishResponse{Phase: phase, AlreadyFinished: again})
}

func key(ref TaskRef) storage.Key { return storage.Key{Namespace: ref.Namespace, Name: ref.Name} }

func validRef(w http.ResponseWriter, ref TaskRef, runID string) bool {
	if ref.Namespace == "" || ref.Name == "" || runID == "" {
		writeError(w, http.StatusBadRequest, CodeInvalid, "task.namespace, task.name and runId are required")
		return false
	}
	return true
}

// decode reads one JSON value from the body. Unknown fields are ignored, so
// the protocol can grow by adding fields without breaking older servers.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	err := dec.Decode(v)
	if err == nil && dec.More() {
		err = errors.New("more than one JSON value")
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, CodeInvalid, fmt.Sprintf("request body over %d bytes", maxBody))
		return false
	case err != nil:
		writeError(w, http.StatusBadRequest, CodeInvalid, fmt.Sprintf("decode request: %v", err))
		return false
	}
	return true
}

// fail maps a Task Manager error to the protocol's status and code. Only
// validation errors, which describe the request itself, are sent in full;
// everything else is answered with its sentinel's text or a fixed message,
// so storage details never reach an unauthenticated client.
func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, task.ErrRunMismatch):
		writeError(w, http.StatusConflict, CodeRunMismatch, task.ErrRunMismatch.Error())
	case errors.Is(err, task.ErrStepMismatch):
		writeError(w, http.StatusConflict, CodeStepMismatch, task.ErrStepMismatch.Error())
	case errors.Is(err, task.ErrInvalidTransition):
		writeError(w, http.StatusConflict, CodeInvalidTransition, task.ErrInvalidTransition.Error())
	case errors.Is(err, manager.ErrInvalid), errors.Is(err, task.ErrInvalidEvent):
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
	case errors.Is(err, storage.ErrNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, "task not found")
	default:
		// Busy objects and storage failures are worth retrying.
		s.log.Warn("runner request failed", "err", err)
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, "temporarily unavailable; retry")
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ErrorResponse{Error: ErrorBody{Code: code, Message: msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
