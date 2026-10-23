package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/task"
)

// Server serves /api/v1 on a Task Manager.
type Server struct {
	m   *manager.Manager
	log *slog.Logger
}

// NewServer returns the API server.
func NewServer(m *manager.Manager, log *slog.Logger) *Server { return &Server{m: m, log: log} }

// Register adds the API's routes to mux.
func (s *Server) Register(mux *http.ServeMux) {
	const tasks = "/api/v1/namespaces/{ns}/tasks"
	const one = tasks + "/{name}"
	mux.HandleFunc("POST "+tasks, s.createTask)
	mux.HandleFunc("GET "+tasks, s.listTasks)
	mux.HandleFunc("GET "+one, s.getTask)
	mux.HandleFunc("POST "+one+"/cancel", s.cancel)
	mux.HandleFunc("POST "+one+"/comments", s.comment)
	mux.HandleFunc("GET "+one+"/thread", s.thread)
	mux.HandleFunc("POST "+one+"/reply", s.reply)
	mux.HandleFunc("GET "+one+"/runs", s.runs)
	mux.HandleFunc("GET "+one+"/runs/{run}/events", s.events)
	mux.HandleFunc("PUT /api/v1/namespaces/{ns}/team", s.initTeam)
	mux.HandleFunc("GET /api/v1/teams", s.listTeams)
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
		s.fail(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, t)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	q := storage.Query{Namespace: r.PathValue("ns")}
	for _, p := range r.URL.Query()["phase"] {
		q.Phases = append(q.Phases, v1alpha1.Phase(p))
	}
	var ok bool
	if q.Offset, ok = intParam(w, r, "offset"); !ok {
		return
	}
	if q.Limit, ok = intParam(w, r, "limit"); !ok {
		return
	}
	items, err := s.m.ListTasks(r.Context(), q)
	if err != nil {
		s.fail(w, err)
		return
	}
	if items == nil {
		items = []v1alpha1.Task{}
	}
	httpjson.Write(w, http.StatusOK, TaskList{Items: items, Offset: q.Offset, Limit: q.Limit})
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.m.GetTask(r.Context(), taskKey(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, t)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	t, err := s.m.Cancel(r.Context(), taskKey(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, t)
}

func (s *Server) comment(w http.ResponseWriter, r *http.Request) {
	var req CommentRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	entry, err := s.m.AppendThread(r.Context(), taskKey(r), manager.Author{Kind: manager.AuthorUser, Name: req.Author}, manager.EntryComment, req.Body)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, entry)
}

func (s *Server) thread(w http.ResponseWriter, r *http.Request) {
	after, ok := intParam(w, r, "after")
	if !ok {
		return
	}
	limit, ok := intParam(w, r, "limit")
	if !ok {
		return
	}
	items, err := s.m.ReadThread(r.Context(), taskKey(r), int64(after), limit)
	if err != nil {
		s.fail(w, err)
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
	t, entry, err := s.m.Answer(r.Context(), taskKey(r), manager.Author{Kind: manager.AuthorUser, Name: req.Author}, req.Body)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, ReplyResponse{Task: t, Entry: entry})
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	items, err := s.m.Runs(r.Context(), taskKey(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, Runs{Items: items})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	items, err := s.m.RunEvents(r.Context(), taskKey(r), r.PathValue("run"))
	if err != nil {
		s.fail(w, err)
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
		s.fail(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, team)
}

func (s *Server) listTeams(w http.ResponseWriter, r *http.Request) {
	items, err := s.m.ListTeams(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, TeamList{Items: items})
}

// intParam reads a non-negative integer query parameter; absent means 0.
func intParam(w http.ResponseWriter, r *http.Request, name string) (int, bool) {
	v := r.URL.Query().Get(name)
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

// fail maps a Task Manager error to a status and code. As on /runner/v1,
// only validation errors, which describe the request, are sent in full;
// storage details stay in the server log.
func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, manager.ErrInvalid), errors.Is(err, task.ErrInvalidEvent), errors.Is(err, storage.ErrInvalidQuery):
		httpjson.WriteError(w, http.StatusBadRequest, httpjson.CodeInvalid, err.Error())
	case errors.Is(err, storage.ErrNotFound):
		httpjson.WriteError(w, http.StatusNotFound, httpjson.CodeNotFound, "not found")
	case errors.Is(err, storage.ErrAlreadyExists):
		httpjson.WriteError(w, http.StatusConflict, httpjson.CodeAlreadyExists, "already exists")
	case errors.Is(err, task.ErrInvalidTransition):
		httpjson.WriteError(w, http.StatusConflict, httpjson.CodeInvalidTransition, task.ErrInvalidTransition.Error())
	default:
		s.log.Warn("api request failed", "err", err)
		httpjson.WriteError(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable, "temporarily unavailable; retry")
	}
}
