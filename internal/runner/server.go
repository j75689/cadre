package runner

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// Server serves /runner/v1 on a Task Manager.
type Server struct {
	m   *manager.Manager
	log *slog.Logger
	// mcpURL is the URL runners reach the per-run MCP endpoint at.
	mcpURL string
	// auth, when set, authenticates every request; builtin is the identity
	// that may claim builtin agents' tasks.
	auth    Authenticator
	builtin string
	dropped sync.Map // identities told their reserved labels were dropped
}

// NewServer returns the protocol server. publicURL is the address runners
// reach cadre-server at; the claim response's MCP URL is built on it.
// Without WithAuth it does not authenticate runners.
func NewServer(m *manager.Manager, log *slog.Logger, publicURL string, opts ...Option) *Server {
	s := &Server{m: m, log: log, mcpURL: strings.TrimRight(publicURL, "/") + "/mcp/run"}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Register adds the protocol's routes to mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+PathClaim, s.authenticated(s.claim))
	mux.HandleFunc("POST "+PathHeartbeat, s.authenticated(s.heartbeat))
	mux.HandleFunc("POST "+PathEvents, s.authenticated(s.events))
	mux.HandleFunc("POST "+PathFinish, s.authenticated(s.finish))
	mux.HandleFunc("POST "+PathStepAppend, s.authenticated(s.builtinOnly(s.stepAppend)))
	mux.HandleFunc("POST "+PathStepAdvance, s.authenticated(s.builtinOnly(s.stepAdvance)))
	mux.HandleFunc("POST "+PathStepRelease, s.authenticated(s.builtinOnly(s.stepRelease)))
	mux.HandleFunc("POST "+PathStepTranscript, s.authenticated(s.builtinOnly(s.stepTranscript)))
}

// builtinOnly wraps a step endpoint: steps are of builtin agents only, run
// by cadre-runner, so on an authenticating server every other identity is
// refused before the task is read.
func (s *Server) builtinOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.auth != nil && identityOf(r) != s.builtin {
			httpjson.WriteError(w, http.StatusForbidden, CodeForbidden, msgStepsBuiltinOnly)
			return
		}
		h(w, r)
	}
}

const msgStepsBuiltinOnly = "only cadre-runner works on steps"

func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	var req ClaimRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	if req.Mode != "" && req.Mode != v1alpha1.RunModeRuntime && req.Mode != v1alpha1.RunModeStep {
		httpjson.WriteError(w, http.StatusBadRequest, CodeInvalid, "mode must be runtime or step")
		return
	}
	if req.Mode == v1alpha1.RunModeStep && s.auth != nil && identityOf(r) != s.builtin {
		// Steps are of builtin agents only, run by cadre-runner.
		httpjson.WriteError(w, http.StatusForbidden, CodeForbidden, msgStepsBuiltinOnly)
		return
	}
	creq := manager.ClaimRequest{Runner: req.Runner, Identity: identityOf(r), Namespaces: req.Namespaces, Labels: s.claimLabels(r, req.Labels), Mode: req.Mode}
	if id := authOf(r); s.auth != nil && id.Name != s.builtin && id.Scope != nil {
		// A runner claims where its Runners say, with their labels; what
		// it sends is not taken.
		creq.Labels, creq.Scope = nil, id.Scope
	}
	c, err := s.m.Claim(r.Context(), creq)
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	if c == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	cfg := s.m.Config()
	t := c.Task
	httpjson.Write(w, http.StatusOK, ClaimResponse{
		Task: ClaimedTask{
			Namespace: t.Namespace, Name: t.Name, Number: t.Status.Number,
			Title: t.Spec.Title, Description: t.Spec.Description, Assignee: t.Spec.Assignee,
			Priority: t.Spec.Priority, ParentRef: t.Spec.ParentRef, Labels: t.Spec.Labels,
		},
		Agent:            claimedAgent(c),
		MCP:              &ClaimedMCP{URL: s.mcpURL, Token: c.MCPToken},
		RunID:            c.RunID,
		Mode:             c.Mode,
		LeaseUntil:       c.LeaseUntil,
		LeaseSeconds:     int(cfg.Lease.Seconds()),
		HeartbeatSeconds: int(cfg.Heartbeat.Seconds()),
		Step:             c.Step,
		Attempt:          c.Attempt,
		MaxAttempts:      maxAttempts(c),
		StepKind:         string(c.Kind),
	})
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	var req HeartbeatRequest
	if !httpjson.Decode(w, r, &req) || !validRef(w, req.Task, req.RunID) {
		return
	}
	until, err := s.m.Heartbeat(r.Context(), key(req.Task), manager.StepRequest{RunID: req.RunID, Step: req.Step, Attempt: req.Attempt, Runner: identityOf(r)})
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.Write(w, http.StatusOK, HeartbeatResponse{LeaseUntil: until, LeaseSeconds: int(s.m.Config().Lease.Seconds())})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	var req EventsRequest
	if !httpjson.Decode(w, r, &req) || !validRef(w, req.Task, req.RunID) {
		return
	}
	err := s.m.RecordEvent(r.Context(), key(req.Task), manager.Event{
		RunID: req.RunID, Seq: req.Seq,
		InputTokens: req.Usage.InputTokens, OutputTokens: req.Usage.OutputTokens,
		CacheReadInputTokens: req.Usage.CacheReadInputTokens, CacheCreationInputTokens: req.Usage.CacheCreationInputTokens,
		ContextTokens: req.ContextTokens, Progress: req.Progress, Runner: identityOf(r),
	})
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) finish(w http.ResponseWriter, r *http.Request) {
	var req FinishRequest
	if !httpjson.Decode(w, r, &req) || !validRef(w, req.Task, req.RunID) {
		return
	}
	if (req.Outcome == v1alpha1.RunOutcomeWaiting) != (req.Waiting != nil) {
		httpjson.WriteError(w, http.StatusBadRequest, CodeInvalid, "waiting is required exactly when the outcome is Waiting")
		return
	}
	mreq := manager.FinishRequest{RunID: req.RunID, Runner: identityOf(r), Step: req.Step, Attempt: req.Attempt, Outcome: req.Outcome}
	if req.Waiting != nil {
		mreq.Reason, mreq.ThreadSeq = req.Waiting.Reason, req.Waiting.ThreadSeq
	}
	for _, st := range req.Subtasks {
		mreq.Subtasks = append(mreq.Subtasks, manager.Subtask{ID: st.ID, Spec: st.Spec})
	}
	phase, again, err := s.m.Finish(r.Context(), key(req.Task), mreq)
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.Write(w, http.StatusOK, FinishResponse{Phase: phase, AlreadyFinished: again})
}

// claimedAgent describes the claimed task's agent, with its model's
// credential as a reference only.
func claimedAgent(c *manager.Claimed) *ClaimedAgent {
	if c.Agent == nil {
		return nil
	}
	a := &ClaimedAgent{Name: c.Agent.Name, Instructions: c.Agent.Spec.Instructions, ToolServers: c.Agent.Spec.ToolRefs, Effort: c.Agent.Spec.Effort}
	if a.Effort == "" {
		a.Effort = v1alpha1.EffortHigh // an Agent stored before the field had its default
	}
	if mp := c.ModelProvider; mp != nil {
		s := mp.Spec
		a.Model = &ClaimedModel{
			Type: s.Type, Model: s.Model, BaseURL: s.BaseURL, Region: s.Region, Project: s.Project, Location: s.Location,
			WorkloadIdentity: s.WorkloadIdentity,
		}
		if s.Auth != nil {
			ref := s.Auth.SecretRef
			a.Model.SecretRef = &ref
		}
	}
	return a
}

func key(ref TaskRef) storage.Key { return storage.Key{Namespace: ref.Namespace, Name: ref.Name} }

func validRef(w http.ResponseWriter, ref TaskRef, runID string) bool {
	if ref.Namespace == "" || ref.Name == "" || runID == "" {
		httpjson.WriteError(w, http.StatusBadRequest, CodeInvalid, "task.namespace, task.name and runId are required")
		return false
	}
	return true
}

func maxAttempts(c *manager.Claimed) int32 {
	if c.Mode != v1alpha1.RunModeStep {
		return 0
	}
	return task.MaxAttempts
}

func stepRequest(r *http.Request, ref StepRef) manager.StepRequest {
	return manager.StepRequest{RunID: ref.RunID, Step: ref.Step, Attempt: ref.Attempt, Runner: identityOf(r)}
}

func (s *Server) stepAppend(w http.ResponseWriter, r *http.Request) {
	var req StepAppendRequest
	if !httpjson.DecodeLimit(w, r, &req, MaxStepAppend) || !validRef(w, req.Task, req.RunID) {
		return
	}
	var entry manager.TranscriptEntry
	if err := json.Unmarshal(req.Entry, &entry); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, CodeInvalid, "entry: "+err.Error())
		return
	}
	got, adopted, err := s.m.AppendStep(r.Context(), key(req.Task), stepRequest(r, req.StepRef), entry)
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.WriteVerbatim(w, http.StatusOK, StepAppendResponse{Entry: got, Adopted: adopted})
}

func (s *Server) stepAdvance(w http.ResponseWriter, r *http.Request) {
	var req StepAdvanceRequest
	if !httpjson.Decode(w, r, &req) || !validRef(w, req.Task, req.RunID) {
		return
	}
	var subtasks []manager.Subtask
	for _, st := range req.Subtasks {
		subtasks = append(subtasks, manager.Subtask{ID: st.ID, Spec: st.Spec})
	}
	t, err := s.m.Advance(r.Context(), key(req.Task), stepRequest(r, req.StepRef), subtasks)
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.Write(w, http.StatusOK, FinishResponse{Phase: t.Status.Phase})
}

func (s *Server) stepRelease(w http.ResponseWriter, r *http.Request) {
	var req StepReleaseRequest
	if !httpjson.Decode(w, r, &req) || !validRef(w, req.Task, req.RunID) {
		return
	}
	if err := s.m.ReleaseStep(r.Context(), key(req.Task), stepRequest(r, req.StepRef), req.Error); err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// stepTranscript reads the run's transcript, for the worker holding its
// current step.
func (s *Server) stepTranscript(w http.ResponseWriter, r *http.Request) {
	var req StepRef
	if !httpjson.Decode(w, r, &req) || !validRef(w, req.Task, req.RunID) {
		return
	}
	entries, err := s.m.StepTranscript(r.Context(), key(req.Task), stepRequest(r, req))
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.WriteVerbatim(w, http.StatusOK, StepTranscript{Entries: entries})
}
