package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/validation"

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
	if s.mode == auth.ModeOIDC && s.policy == nil {
		// With logins and no roles everyone would be an admin who has
		// no name: a wiring mistake.
		panic("api: WithAccess with logins needs a policy")
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
		{"GET " + Base + "/namespaces/{ns}/team", authz.Read, s.getTeam},
		{"PUT " + Base + "/namespaces/{ns}/team", authz.ManageTeam, s.putTeam},
		{"GET " + Base + "/namespaces/{ns}/agents", authz.Read, s.listAgents},
	} {
		mux.HandleFunc(route.pattern, s.guard(route.action, route.h))
	}
	mux.HandleFunc("GET "+Base+"/teams", s.listTeams)
	mux.HandleFunc("GET "+Base+"/me", s.me)
}

// guard lets a request through when Check allows action on the team of
// its namespace.
func (s *Server) guard(action authz.Action, h http.HandlerFunc) http.HandlerFunc {
	if s.policy == nil {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var refused *Refused
		switch err := Check(r.Context(), s.m, s.policy, r.PathValue("ns"), action); {
		case errors.Is(err, ErrUnauthenticated):
			httpjson.WriteError(w, http.StatusUnauthorized, httpjson.CodeUnauthenticated, "log in first")
		case errors.As(err, &refused):
			httpjson.WriteError(w, http.StatusForbidden, httpjson.CodeNoRole, refused.Message)
		case err != nil:
			httpjson.Fail(w, s.log, err)
		default:
			h(w, r)
		}
	}
}

// ErrUnauthenticated is a request with no identity where logins are on.
var ErrUnauthenticated = errors.New("log in first")

// Refused is an action the caller's role does not allow; its message names
// the role it needs and the caller's.
type Refused struct{ Message string }

func (r *Refused) Error() string { return r.Message }

// Check decides whether the caller of ctx may take action on the team of
// namespace ns, under policy (nil: everyone may do anything). A team the
// caller has no role on, or that does not exist, is a refusal, so its
// existence is not told; a bootstrap admin passes, and what is not there is
// answered later as not found. The board index decides first, seconds
// behind the store at worst: a refusal there is final, since a lagging
// index can grant too much, never too little. A write the index allows is
// confirmed on the Team read from the store, so a role taken away fences
// the next write, and only someone the index already lets write costs a
// store read. The API's routes and the MCP endpoint's tools all decide
// here.
func Check(ctx context.Context, m *manager.Manager, policy *authz.Policy, ns string, action authz.Action) error {
	if policy == nil {
		return nil
	}
	id, ok := auth.FromContext(ctx)
	if !ok {
		return ErrUnauthenticated
	}
	reads := []func(context.Context, string) (*v1alpha1.Team, error){m.Team}
	if action != authz.Read {
		reads = append(reads, m.LiveTeam)
	}
	for _, read := range reads {
		team, err := read(ctx, ns)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			team = nil
		case err != nil:
			return err
		}
		if role, _ := policy.Role(id, team); !authz.Allowed(role, action) {
			return &Refused{Message: authz.Refusal(ns, role, action)}
		}
	}
	return nil
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
		q.Phases = append(q.Phases, v1alpha1.Phase(p))
	}
	if err := ValidatePhases(q.Phases); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, httpjson.CodeInvalid, err.Error())
		return
	}
	for _, name := range []string{"parent", "number", "offset", "limit"} {
		if n := len(query[name]); n > 1 {
			httpjson.WriteError(w, http.StatusBadRequest, httpjson.CodeInvalid, fmt.Sprintf("%s given %d times: at most once", name, n))
			return
		}
	}
	q.Parent = query.Get("parent")
	if err := ValidateParent(q.Parent); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, httpjson.CodeInvalid, err.Error())
		return
	}
	var ok bool
	if q.Offset, ok = intParam(w, query, "offset"); !ok {
		return
	}
	if q.Limit, ok = intParam(w, query, "limit"); !ok {
		return
	}
	if v := query.Get("number"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			httpjson.WriteError(w, http.StatusBadRequest, httpjson.CodeInvalid, "number must be a task number, an integer of at least 1")
			return
		}
		q.Number = n
	}
	// The response says which limit applied.
	q.Limit = ClampLimit(q.Limit)
	items, err := s.tasks(r.Context(), q)
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	if items == nil {
		items = []v1alpha1.Task{}
	}
	httpjson.Write(w, http.StatusOK, TaskList{Items: items, Offset: q.Offset, Limit: q.Limit})
}

// tasks answers a list query. One by number is the task the Task Manager
// finds by it, which allows for the index's lag, with the query's other
// filters and its page applied: zero tasks or one.
func (s *Server) tasks(ctx context.Context, q storage.Query) ([]v1alpha1.Task, error) {
	if q.Number == 0 {
		return s.m.ListTasks(ctx, q)
	}
	t, err := s.m.TaskByNumber(ctx, q.Namespace, q.Number)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	case !q.Matches(t):
		return nil, nil
	}
	return storage.Page(q, []v1alpha1.Task{*t}), nil
}

// ValidatePhases refuses a phase that is not a Task's, naming the ones
// that are: an unknown one would match nothing, and look like an empty
// board.
func ValidatePhases(ps []v1alpha1.Phase) error {
	for _, p := range ps {
		if !slices.Contains(phases, p) {
			return fmt.Errorf("phase %q: not one of %v", p, phases)
		}
	}
	return nil
}

// ValidateParent refuses a parent that cannot be a task's name: a task name
// is a DNS subdomain, so a parent's is. Empty is no parent.
func ValidateParent(parent string) error {
	if parent == "" {
		return nil
	}
	if errs := validation.IsDNS1123Subdomain(parent); len(errs) > 0 {
		return fmt.Errorf("parent %q: %s", parent, strings.Join(errs, "; "))
	}
	return nil
}

// ClampLimit is the page size a board read applies to the limit asked for:
// none means DefaultLimit, and more than MaxLimit is MaxLimit.
func ClampLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultLimit
	case limit > MaxLimit:
		return MaxLimit
	}
	return limit
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

// getTeam answers a namespace's Team, as GET /teams shows it to the caller.
func (s *Server) getTeam(w http.ResponseWriter, r *http.Request) {
	team, err := s.m.LiveTeam(r.Context(), r.PathValue("ns"))
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	httpjson.Write(w, http.StatusOK, s.project(r.Context(), team))
}

// putTeam creates a namespace's Team, for a bootstrap admin (the guard
// lets nobody else through where there is none), or replaces the spec of
// the one there, for an admin of it, from the version the request read. A
// request without a version is a creation: on an existing Team it is 409
// already_exists, as cadre team init has always been told.
func (s *Server) putTeam(w http.ResponseWriter, r *http.Request) {
	var req TeamRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	ns := r.PathValue("ns")
	cur, err := s.m.LiveTeam(r.Context(), ns)
	switch {
	case errors.Is(err, storage.ErrNotFound) && req.ResourceVersion != "":
		// An edit of a Team deleted since it was read: not a creation.
		// Not ErrTeamChanged's advice to read it again: a read is a 404.
		httpjson.WriteError(w, http.StatusConflict, httpjson.CodeConflict, fmt.Sprintf("the Team of %s was deleted since it was read: there is nothing to edit", ns))
		return
	case errors.Is(err, storage.ErrNotFound):
		team, err := s.m.InitTeam(r.Context(), ns, req.TeamSpec)
		if err != nil {
			httpjson.Fail(w, s.log, err)
			return
		}
		httpjson.Write(w, http.StatusCreated, team)
		return
	case err != nil:
		httpjson.Fail(w, s.log, err)
		return
	case req.ResourceVersion == "":
		httpjson.Fail(w, s.log, fmt.Errorf("team %s: %w", ns, storage.ErrAlreadyExists))
		return
	}
	spec := req.TeamSpec
	if err := manager.ValidateTeamSpec(&spec); err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	if s.orphans(cur, spec.Roles) {
		httpjson.WriteError(w, http.StatusBadRequest, httpjson.CodeInvalid,
			"the Team would have no admin, and no bootstrap admin is set to give it one back: keep an admin role")
		return
	}
	team, err := s.m.UpdateTeam(r.Context(), cur, spec, req.ResourceVersion)
	if err != nil {
		httpjson.Fail(w, s.log, err)
		return
	}
	// As the edit left the caller: an admin who gave the role away sees
	// what anyone else does.
	httpjson.Write(w, http.StatusOK, s.project(r.Context(), team))
}

// orphans reports whether roles would leave a Team that has an admin with
// none, where no bootstrap admin is set who could give it one back.
func (s *Server) orphans(cur *v1alpha1.Team, roles []v1alpha1.TeamRole) bool {
	if s.policy == nil || len(s.policy.AdminUsers) > 0 || len(s.policy.AdminGroups) > 0 {
		return false
	}
	hasAdmin := func(roles []v1alpha1.TeamRole) bool {
		return slices.ContainsFunc(roles, func(r v1alpha1.TeamRole) bool { return r.Role == v1alpha1.RoleAdmin && len(r.Subjects) > 0 })
	}
	return hasAdmin(cur.Spec.Roles) && !hasAdmin(roles)
}

// project is t as the caller may see it: its roles, status, annotations and
// fields' owners only for an admin of it, since the roles show in all of
// them (kubectl apply keeps the last spec in an annotation).
func (s *Server) project(ctx context.Context, t *v1alpha1.Team) *v1alpha1.Team {
	if s.policy == nil {
		return t
	}
	id, _ := auth.FromContext(ctx)
	if role, _ := s.policy.Role(id, t); role < authz.Admin {
		t.Spec.Roles, t.Status = nil, v1alpha1.TeamStatus{}
		t.Annotations, t.ManagedFields = nil, nil
	}
	return t
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
		visible := items[:0]
		for _, t := range items {
			if _, ok := s.policy.Role(id, &t); !ok {
				continue
			}
			visible = append(visible, *s.project(r.Context(), &t))
		}
		items = visible
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
