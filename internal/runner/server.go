package runner

import (
	"log/slog"
	"net/http"
	"strings"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
)

// Server serves /runner/v1 on a Task Manager. In M0 it is unauthenticated.
type Server struct {
	m   *manager.Manager
	log *slog.Logger
	// mcpBase is the URL runners reach the per-run MCP endpoint at,
	// without the token.
	mcpBase string
}

// NewServer returns the protocol server. publicURL is the address runners
// reach cadre-server at; the claim response's MCP URL is built on it.
func NewServer(m *manager.Manager, log *slog.Logger, publicURL string) *Server {
	return &Server{m: m, log: log, mcpBase: strings.TrimRight(publicURL, "/") + "/mcp/run/"}
}

// Register adds the protocol's routes to mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+PathClaim, s.claim)
	mux.HandleFunc("POST "+PathHeartbeat, s.heartbeat)
	mux.HandleFunc("POST "+PathEvents, s.events)
	mux.HandleFunc("POST "+PathFinish, s.finish)
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	var req ClaimRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	c, err := s.m.Claim(r.Context(), manager.ClaimRequest{Runner: req.Runner, Namespaces: req.Namespaces, Labels: req.Labels})
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
		MCP:              &ClaimedMCP{URL: s.mcpBase + c.MCPToken},
		RunID:            c.RunID,
		Mode:             c.Mode,
		LeaseUntil:       c.LeaseUntil,
		LeaseSeconds:     int(cfg.Lease.Seconds()),
		HeartbeatSeconds: int(cfg.Heartbeat.Seconds()),
	})
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	var req HeartbeatRequest
	if !httpjson.Decode(w, r, &req) || !validRef(w, req.Task, req.RunID) {
		return
	}
	until, err := s.m.Heartbeat(r.Context(), key(req.Task), req.RunID)
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
		ContextTokens: req.ContextTokens, Progress: req.Progress,
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
	mreq := manager.FinishRequest{RunID: req.RunID, Step: req.Step, Outcome: req.Outcome}
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
	a := &ClaimedAgent{Name: c.Agent.Name, Instructions: c.Agent.Spec.Instructions, ToolServers: c.Agent.Spec.ToolRefs}
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
