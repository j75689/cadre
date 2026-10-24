package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
)

// Server serves /api/v1 on a Task Manager.
type Server struct {
	m      *manager.Manager
	log    *slog.Logger
	mode   string        // auth.ModeOIDC or auth.ModeNone
	policy *authz.Policy // nil: everyone may do anything
}

// Option adjusts NewServer.
type Option func(*Server)

// WithAccess says how people log in (auth.ModeOIDC or auth.ModeNone), for
// /me, and enforces p's roles on every route when p is set; requests must
// then carry an identity (auth.Authenticator.Wrap gives one).
func WithAccess(mode string, p *authz.Policy) Option {
	return func(s *Server) { s.mode, s.policy = mode, p }
}

// NewServer returns the API server.
func NewServer(m *manager.Manager, log *slog.Logger, opts ...Option) *Server {
	s := &Server{m: m, log: log, mode: auth.ModeNone}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Register adds the API's routes to mux.
func (s *Server) Register(mux *http.ServeMux) {
	const tasks = Base + "/namespaces/{ns}/tasks"
	const one = tasks + "/{name}"
	// Every route under a namespace names what it does on the team's board,
	// and the policy decides whether the caller may.
	for _, route := range []struct {
		pattern string
		action  authz.Action
		h       http.HandlerFunc
	}{
		{"POST " + tasks, authz.CreateTask, s.createTask},
		{"GET " + tasks, authz.Read, s.listTasks},
		{"GET " + one, authz.Read, s.getTask},
		{"POST " + one + "/cancel", authz.Cancel, s.cancel},
		{"POST " + one + "/comments", authz.Comment, s.comment},
		{"GET " + one + "/thread", authz.Read, s.thread},
		{"POST " + one + "/reply", authz.Reply, s.reply},
		{"POST " + one + approveSuffix, authz.Approve, s.approve},
		{"GET " + one + "/runs", authz.Read, s.runs},
		{"GET " + one + "/runs/{run}/events", authz.Read, s.events},
		// Creating a Team needs a bootstrap admin: nobody has a role on a
		// team that does not exist.
		{"PUT " + Base + "/namespaces/{ns}/team", authz.ManageTeam, s.initTeam},
		{"GET " + Base + "/namespaces/{ns}/agents", authz.Read, s.listAgents},
	} {
		mux.HandleFunc(route.pattern, s.guard(route.action, route.h))
	}
	mux.HandleFunc("GET "+Base+"/teams", s.listTeams)
	mux.HandleFunc("GET "+Base+"/me", s.me)
}

// guard lets a request through when the caller's role on the team of its
// namespace allows action. A team the caller has no role on, or that does
// not exist, is refused with 403, so its existence is not told; a bootstrap
// admin passes, and the handler answers 404 for what is not there. A read
// decides on the board index, seconds behind at worst; anything else on the
// Team read from the store, so a role taken away fences the next write.
func (s *Server) guard(action authz.Action, h http.HandlerFunc) http.HandlerFunc {
	if s.policy == nil {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.FromContext(r.Context())
		if !ok {
			httpjson.WriteError(w, http.StatusUnauthorized, httpjson.CodeUnauthenticated, "log in first")
			return
		}
		ns := r.PathValue("ns")
		read := s.m.LiveTeam
		if action == authz.Read {
			read = s.m.Team
		}
		team, err := read(r.Context(), ns)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			team = nil
		case err != nil:
			httpjson.Fail(w, s.log, err)
			return
		}
		role, _ := s.policy.Role(id, team)
		if !authz.Allowed(role, action) {
			httpjson.WriteError(w, http.StatusForbidden, httpjson.CodeNoRole, authz.Refusal(ns, role, action))
			return
		}
		h(w, r)
	}
}

// me says who the caller is and their role on each team they may see.
// Without logins nobody is known and everyone is an admin.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if s.mode != auth.ModeOIDC || s.policy == nil || !ok {
		httpjson.Write(w, http.StatusOK, auth.Me{Mode: s.mode, Admin: true})
		return
	}
	teams, err := s.m.ListTeams(r.Context())
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	out := auth.Me{Mode: auth.ModeOIDC, Identity: id, Admin: s.policy.Bootstrap(id), Roles: map[string]string{}}
	for i := range teams {
		if role, ok := s.policy.Role(id, &teams[i]); ok {
			out.Roles[teams[i].Namespace] = role.String()
		}
	}
	httpjson.Write(w, http.StatusOK, out)
}

func taskKey(r *http.Request) storage.Key {
	return storage.Key{Namespace: r.PathValue("ns"), Name: r.PathValue("name")}
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req CreateTaskRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	t, err := s.m.CreateTask(r.Context(), r.PathValue("ns"), req.Name, req.Spec)
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, t)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	q := storage.Query{Namespace: r.PathValue("ns")}
	for _, p := range query["phase"] {
		if !slices.Contains(phases, v1alpha1.Phase(p)) {
			httpjson.WriteError(w, http.StatusBadRequest, httpjson.CodeInvalid, fmt.Sprintf("phase %q: not one of %v", p, phases))
			return
		}
		q.Phases = append(q.Phases, v1alpha1.Phase(p))
	}
	var ok bool
	if q.Offset, ok = intParam(w, query, "offset"); !ok {
		return
	}
	if q.Limit, ok = intParam(w, query, "limit"); !ok {
		return
	}
	// A board page is bounded: no limit means the default, and a larger
	// one is capped. The response says which limit applied.
	switch {
	case q.Limit == 0:
		q.Limit = DefaultLimit
	case q.Limit > MaxLimit:
		q.Limit = MaxLimit
	}
	items, err := s.m.ListTasks(r.Context(), q)
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	if items == nil {
		items = []v1alpha1.Task{}
	}
	httpjson.Write(w, http.StatusOK, TaskList{Items: items, Offset: q.Offset, Limit: q.Limit})
}

var phases = []v1alpha1.Phase{
	v1alpha1.PhaseTodo, v1alpha1.PhaseRunning, v1alpha1.PhaseWaiting, v1alpha1.PhaseReview,
	v1alpha1.PhaseDone, v1alpha1.PhaseCancelled, v1alpha1.PhaseFailed,
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.m.GetTask(r.Context(), taskKey(r))
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.Write(w, http.StatusOK, t)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	t, err := s.m.Cancel(r.Context(), taskKey(r))
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.Write(w, http.StatusOK, t)
}

func (s *Server) comment(w http.ResponseWriter, r *http.Request) {
	var req CommentRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	entry, err := s.m.AppendThread(r.Context(), taskKey(r), manager.Author{Kind: manager.AuthorUser, Name: auth.Author(r, req.Author)}, manager.EntryComment, req.Body)
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, entry)
}

func (s *Server) thread(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	after, ok := intParam(w, query, "after")
	if !ok {
		return
	}
	limit, ok := intParam(w, query, "limit")
	if !ok {
		return
	}
	items, err := s.m.ReadThread(r.Context(), taskKey(r), int64(after), limit)
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.Write(w, http.StatusOK, Thread{Items: items})
}

// reply answers a waiting task. A task that is not Waiting is refused with
// 409 before anything is written; see manager.Answer for the narrow race in
// which the answer stays in the thread as a comment.
func (s *Server) reply(w http.ResponseWriter, r *http.Request) {
	var req CommentRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	t, entry, err := s.m.Answer(r.Context(), taskKey(r), manager.Author{Kind: manager.AuthorUser, Name: auth.Author(r, req.Author)}, req.Body)
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.Write(w, http.StatusOK, ReplyResponse{Task: t, Entry: entry})
}

const approveSuffix = "/approve"

// IsApproval reports whether r approves or denies a tool call, which may
// make the call and so needs the longer request bound.
func IsApproval(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, Base+"/namespaces/") && strings.HasSuffix(r.URL.Path, approveSuffix)
}

// approve decides the tool call a task waits on; see manager.Approve.
func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var req ApproveRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	if req.Approve == nil {
		httpjson.WriteError(w, http.StatusBadRequest, httpjson.CodeInvalid, "approve is required: true to approve, false to deny")
		return
	}
	a, err := s.m.Approve(r.Context(), taskKey(r), manager.Author{Kind: manager.AuthorUser, Name: auth.Author(r, req.Author)}, *req.Approve, req.Note)
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.Write(w, http.StatusOK, ApproveResponse{Task: a.Task, Decision: a.Decision, Result: a.Result})
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	items, err := s.m.Runs(r.Context(), taskKey(r))
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.Write(w, http.StatusOK, Runs{Items: items})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	items, err := s.m.RunEvents(r.Context(), taskKey(r), r.PathValue("run"))
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.Write(w, http.StatusOK, Events{Items: items})
}

func (s *Server) initTeam(w http.ResponseWriter, r *http.Request) {
	var req TeamRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	team, err := s.m.InitTeam(r.Context(), r.PathValue("ns"), v1alpha1.TeamSpec{DisplayName: req.DisplayName, MaxConcurrentRuns: req.MaxConcurrentRuns})
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, team)
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.m.ListAgents(r.Context(), r.PathValue("ns"))
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	items := make([]AgentSummary, 0, len(agents))
	for _, a := range agents {
		items = append(items, summarize(&a))
	}
	httpjson.Write(w, http.StatusOK, AgentList{Items: items})
}

// summarize says whether a can take work. A Ready condition decided for an
// older spec says nothing about this one.
func summarize(a *v1alpha1.Agent) AgentSummary {
	out := AgentSummary{Name: a.Name, Ready: a.Status.ReadyFor(a.Generation)}
	c := meta.FindStatusCondition(a.Status.Conditions, v1alpha1.ConditionReady)
	switch {
	case c == nil || c.ObservedGeneration != a.Generation || a.Status.ObservedGeneration != a.Generation:
		out.Reason, out.Message = "Pending", "its current spec has not been checked yet"
	case !out.Ready:
		out.Reason, out.Message = c.Reason, c.Message
	}
	return out
}

// listTeams lists the Teams the caller has a role on: every one, without
// logins or for a bootstrap admin. Who holds which role, and the Team's
// control state, are an admin's to see only.
func (s *Server) listTeams(w http.ResponseWriter, r *http.Request) {
	items, err := s.m.ListTeams(r.Context())
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	if s.policy != nil {
		id, _ := auth.FromContext(r.Context())
		items = slices.DeleteFunc(items, func(t v1alpha1.Team) bool {
			_, ok := s.policy.Role(id, &t)
			return !ok
		})
		for i := range items {
			if role, _ := s.policy.Role(id, &items[i]); role < authz.Admin {
				items[i].Spec.Roles, items[i].Status = nil, v1alpha1.TeamStatus{}
			}
		}
	}
	if items == nil {
		items = []v1alpha1.Team{}
	}
	httpjson.Write(w, http.StatusOK, TeamList{Items: items})
}

// intParam reads a non-negative integer query parameter; absent means 0.
func intParam(w http.ResponseWriter, query url.Values, name string) (int, bool) {
	v := query.Get(name)
	if v == "" {
		return 0, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		httpjson.WriteError(w, http.StatusBadRequest, httpjson.CodeInvalid, name+" must be a non-negative integer")
		return 0, false
	}
	return n, true
}
