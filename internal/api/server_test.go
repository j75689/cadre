package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
)

const ns = "team-a"

var ctx = context.Background()

func start(t *testing.T) (*client.Client, *manager.Manager) {
	c, m, _ := startURL(t)
	return c, m
}

func startURL(t *testing.T) (*client.Client, *manager.Manager, string) {
	t.Helper()
	store := memory.NewBackend().Open()
	m := manager.New(store, manager.Config{Agents: manager.ReadyBuiltin(ns, "coder", "claude")})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	api.NewServer(m, log).Register(mux)
	runner.NewServer(m, log, "http://cadre.test").Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return client.New(srv.URL, nil), m, srv.URL
}

func agent() *v1alpha1.Assignee {
	return &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}
}

func TestBoardThroughTheAPI(t *testing.T) {
	c, m := start(t)

	if _, err := c.CreateTask(ctx, ns, "", v1alpha1.TaskSpec{Title: "x"}); !client.IsCode(err, httpjson.CodeNotFound) {
		t.Fatalf("create without a Team: %v", err)
	}
	if _, err := c.InitTeam(ctx, ns, api.TeamRequest{TeamSpec: v1alpha1.TeamSpec{DisplayName: "Backend", MaxConcurrentRuns: 2}}); err != nil {
		t.Fatalf("init team: %v", err)
	}
	if _, err := c.InitTeam(ctx, ns, api.TeamRequest{}); !client.IsCode(err, httpjson.CodeAlreadyExists) {
		t.Fatalf("init team twice: %v", err)
	}
	if teams, err := c.ListTeams(ctx); err != nil || len(teams.Items) != 1 || teams.Items[0].Spec.DisplayName != "Backend" {
		t.Fatalf("teams: %+v, %v", teams, err)
	}

	tk, err := c.CreateTask(ctx, ns, "fix-login", v1alpha1.TaskSpec{Title: "Fix login", Assignee: agent(), Priority: v1alpha1.PriorityHigh})
	if err != nil || tk.Status.Number != 1 || tk.Status.Phase != v1alpha1.PhaseTodo {
		t.Fatalf("create: %+v, %v", tk, err)
	}
	if _, err := c.CreateTask(ctx, ns, "", v1alpha1.TaskSpec{Title: "Later"}); err != nil {
		t.Fatal(err)
	}
	list, err := c.ListTasks(ctx, ns, client.TaskQuery{})
	if err != nil || len(list.Items) != 2 || list.Items[0].Name != "fix-login" {
		t.Fatalf("list: %+v, %v", list, err)
	}
	if page, _ := c.ListTasks(ctx, ns, client.TaskQuery{Phases: []v1alpha1.Phase{v1alpha1.PhaseRunning}}); len(page.Items) != 0 {
		t.Fatalf("Running tasks: %+v", page.Items)
	}

	if entry, err := c.Comment(ctx, ns, "fix-login", "alice", "it fails on Mondays"); err != nil || entry.Seq != 1 || entry.Author.Kind != manager.AuthorUser {
		t.Fatalf("comment: %+v, %v", entry, err)
	}
	if _, err := c.Reply(ctx, ns, "fix-login", "alice", "not waiting"); !client.IsCode(err, httpjson.CodeInvalidTransition) {
		t.Fatalf("reply to a Todo task: %v", err)
	}

	claim, err := c.Claim(ctx, runner.ClaimRequest{Runner: "r", Labels: map[string]string{v1alpha1.LabelRuntime: string(v1alpha1.RuntimeBuiltin)}})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	ref := runner.TaskRef{Namespace: ns, Name: claim.Task.Name}
	if err := c.Event(ctx, runner.EventsRequest{Task: ref, RunID: claim.RunID, Seq: 1, Progress: "reading"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Finish(ctx, runner.FinishRequest{Task: ref, RunID: claim.RunID, Outcome: v1alpha1.RunOutcomeWaiting, Waiting: &runner.Waiting{Reason: v1alpha1.WaitingQuestion}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}

	reply, err := c.Reply(ctx, ns, "fix-login", "alice", "use staging")
	if err != nil || reply.Task.Status.Phase != v1alpha1.PhaseTodo || reply.Entry.Seq != 2 {
		t.Fatalf("reply: %+v, %v", reply, err)
	}
	thread, err := c.Thread(ctx, ns, "fix-login", 0, 0)
	if err != nil || len(thread.Items) != 2 || thread.Items[1].Kind != manager.EntryAnswer {
		t.Fatalf("thread: %+v, %v", thread, err)
	}
	runs, err := c.Runs(ctx, ns, "fix-login")
	if err != nil || len(runs.Items) != 1 || runs.Items[0].Outcome != v1alpha1.RunOutcomeWaiting {
		t.Fatalf("runs: %+v, %v", runs, err)
	}
	events, err := c.Events(ctx, ns, "fix-login", claim.RunID)
	if err != nil || len(events.Items) != 1 {
		t.Fatalf("events: %+v, %v", events, err)
	}

	got, err := c.CancelTask(ctx, ns, "fix-login")
	if err != nil || got.Status.Phase != v1alpha1.PhaseCancelled {
		t.Fatalf("cancel: %+v, %v", got, err)
	}
	if _, err := c.GetTask(ctx, ns, "missing"); !client.IsCode(err, httpjson.CodeNotFound) {
		t.Fatalf("get missing: %v", err)
	}
}

func TestAPIRejectsBadInput(t *testing.T) {
	c, m, base := startURL(t)
	if _, err := m.InitTeam(ctx, ns, v1alpha1.TeamSpec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateTask(ctx, ns, "", v1alpha1.TaskSpec{}); !client.IsCode(err, httpjson.CodeInvalid) {
		t.Errorf("create without a title: %v", err)
	}
	if _, err := c.CreateTask(ctx, ns, "t", v1alpha1.TaskSpec{Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Comment(ctx, ns, "t", "", "x"); !client.IsCode(err, httpjson.CodeInvalid) {
		t.Errorf("comment without an author: %v", err)
	}
	if _, err := c.Events(ctx, ns, "t", "not-a-ulid"); !client.IsCode(err, httpjson.CodeInvalid) {
		t.Errorf("events for a bad run id: %v", err)
	}
	if _, err := c.InitTeam(ctx, "team-b", api.TeamRequest{TeamSpec: v1alpha1.TeamSpec{MaxConcurrentRuns: -1}}); !client.IsCode(err, httpjson.CodeInvalid) {
		t.Errorf("negative maxConcurrentRuns: %v", err)
	}
	if _, err := c.InitTeam(ctx, "Team_B", api.TeamRequest{}); !client.IsCode(err, httpjson.CodeInvalid) {
		t.Errorf("bad namespace: %v", err)
	}
	if _, err := c.CreateTask(ctx, ns, "Fix_Login", v1alpha1.TaskSpec{Title: "x"}); !client.IsCode(err, httpjson.CodeInvalid) {
		t.Errorf("bad task name: %v", err)
	}
	if _, err := c.ListTasks(ctx, ns, client.TaskQuery{Phases: []v1alpha1.Phase{"Sleeping"}}); !client.IsCode(err, httpjson.CodeInvalid) {
		t.Errorf("unknown phase: %v", err)
	}
	if _, err := c.ListTasks(ctx, ns, client.TaskQuery{Parent: "Not_A_Name"}); !client.IsCode(err, httpjson.CodeInvalid) {
		t.Errorf("a parent that is not a task name: %v", err)
	}
	// A task name is a DNS subdomain: dots, and more than 63 characters.
	for _, parent := range []string{"release.2026", strings.Repeat("a", 64)} {
		if _, err := c.ListTasks(ctx, ns, client.TaskQuery{Parent: parent}); err != nil {
			t.Errorf("parent %q: %v", parent, err)
		}
	}
	for _, q := range []string{"limit=-1", "parent=-a", "parent=" + strings.Repeat("a", 254), "parent=a&parent=b", "offset=1&offset=2", "limit=1&limit=2"} {
		resp, err := http.Get(base + "/api/v1/namespaces/" + ns + "/tasks?" + q)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d", q, resp.StatusCode)
		}
	}
}

// parent lists a task's subtasks, with the other parameters applied to
// them.
func TestListSubtasks(t *testing.T) {
	c, m := start(t)
	if _, err := m.InitTeam(ctx, ns, v1alpha1.TeamSpec{}); err != nil {
		t.Fatal(err)
	}
	for _, tk := range []struct{ name, parent string }{{"epic", ""}, {"step-1", "epic"}, {"step-2", "epic"}, {"step-3", "epic"}, {"other", "elsewhere"}} {
		spec := v1alpha1.TaskSpec{Title: tk.name}
		if tk.parent != "" {
			spec.ParentRef = &v1alpha1.TaskReference{Name: tk.parent}
		}
		if _, err := c.CreateTask(ctx, ns, tk.name, spec); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.CancelTask(ctx, ns, "step-2"); err != nil {
		t.Fatal(err)
	}
	names := func(q client.TaskQuery) []string {
		t.Helper()
		list, err := c.ListTasks(ctx, ns, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, tk := range list.Items {
			out = append(out, tk.Name)
		}
		return out
	}
	for _, tc := range []struct {
		q    client.TaskQuery
		want string
	}{
		{client.TaskQuery{Parent: "epic"}, "step-1 step-2 step-3"},
		{client.TaskQuery{Parent: "epic", Phases: []v1alpha1.Phase{v1alpha1.PhaseCancelled}}, "step-2"},
		{client.TaskQuery{Parent: "epic", Offset: 1, Limit: 1}, "step-2"},
		{client.TaskQuery{Parent: "step-1"}, ""},
	} {
		if got := strings.Join(names(tc.q), " "); got != tc.want {
			t.Errorf("%+v: got %q, want %q", tc.q, got, tc.want)
		}
	}
}

// number finds the one task with it, with the other filters applied to
// it; a number nobody has finds nothing, and one that is not a task
// number is refused.
func TestListByNumber(t *testing.T) {
	c, m, base := startURL(t)
	if _, err := m.InitTeam(ctx, ns, v1alpha1.TeamSpec{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		if _, err := c.CreateTask(ctx, ns, name, v1alpha1.TaskSpec{Title: name}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		q    client.TaskQuery
		want string
	}{
		{client.TaskQuery{Number: 2}, "second"},
		{client.TaskQuery{Number: 2, Phases: []v1alpha1.Phase{v1alpha1.PhaseTodo}}, "second"},
		{client.TaskQuery{Number: 2, Phases: []v1alpha1.Phase{v1alpha1.PhaseDone}}, ""},
		{client.TaskQuery{Number: 2, Offset: 1}, ""},
		{client.TaskQuery{Number: 3}, ""},
	} {
		list, err := c.ListTasks(ctx, ns, tc.q)
		var got []string
		for _, tk := range list.Items {
			got = append(got, tk.Name)
		}
		if err != nil || strings.Join(got, " ") != tc.want || list.Items == nil {
			t.Errorf("%+v: %q, %v; want %q", tc.q, got, err, tc.want)
		}
	}
	for _, q := range []string{"number=0", "number=-1", "number=two", "number=1&number=2"} {
		resp, err := http.Get(base + "/api/v1/namespaces/" + ns + "/tasks?" + q)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d", q, resp.StatusCode)
		}
	}
}

func TestListTasksPageSize(t *testing.T) {
	c, m := start(t)
	if _, err := m.InitTeam(ctx, ns, v1alpha1.TeamSpec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateTask(ctx, ns, "t", v1alpha1.TaskSpec{Title: "t"}); err != nil {
		t.Fatal(err)
	}
	for limit, want := range map[int]int{0: api.DefaultLimit, 7: 7, 10000: api.MaxLimit} {
		list, err := c.ListTasks(ctx, ns, client.TaskQuery{Limit: limit})
		if err != nil || list.Limit != want || len(list.Items) != 1 {
			t.Errorf("limit %d: %+v, %v; want limit %d applied", limit, list, err, want)
		}
	}
}

// A base URL with a trailing slash must not produce "//" paths. cadre-server
// itself answers those with a 307 that keeps the method, but a proxy or
// ingress in front of it may answer 301, and a client following that turns
// a POST into a body-less GET that decodes the wrong response as success.
func TestClientIgnoresATrailingSlash(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		httpjson.Write(w, http.StatusCreated, v1alpha1.Task{})
	}))
	defer srv.Close()
	c := client.New(srv.URL+"/", nil)
	if _, err := c.CreateTask(ctx, ns, "slash", v1alpha1.TaskSpec{Title: "slash"}); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != api.Base+"/namespaces/"+ns+"/tasks" {
		t.Fatalf("requested %v, want one request to the clean path", paths)
	}
}

func TestApproveThroughTheAPI(t *testing.T) {
	c, m, base := startURL(t)
	if _, err := c.InitTeam(ctx, ns, api.TeamRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateTask(ctx, ns, "open-pr", v1alpha1.TaskSpec{Title: "Open a PR", Assignee: agent()}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Approve(ctx, ns, "open-pr", "alice", true, ""); !client.IsCode(err, httpjson.CodeInvalidTransition) {
		t.Fatalf("approve a Todo task: %v", err)
	}
	claim, err := c.Claim(ctx, runner.ClaimRequest{Runner: "r", Labels: map[string]string{v1alpha1.LabelRuntime: string(v1alpha1.RuntimeBuiltin)}})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	key := storage.Key{Namespace: ns, Name: "open-pr"}
	if _, err := m.RunToolCall(ctx, key, claim.RunID, manager.ToolCall{Name: "github__create_pull_request", ToolServer: "github", Tool: "create_pull_request"}); err != nil {
		t.Fatal(err)
	}

	// Leaving approve out must not deny.
	res, err := http.Post(base+api.Base+"/namespaces/"+ns+"/tasks/open-pr/approve", "application/json", strings.NewReader(`{"author":"alice"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("approve without a decision: %d, want 400", res.StatusCode)
	}
	if _, _, err := m.Answer(ctx, key, manager.Author{Kind: manager.AuthorUser, Name: "alice"}, "yes"); err == nil {
		t.Fatal("a reply must not end a tool approval")
	}

	out, err := c.Approve(ctx, ns, "open-pr", "alice", false, "not yet")
	if err != nil || out.Decision.Kind != manager.EntryDenied || out.Decision.Body != "not yet" || out.Result != nil || out.Task.Status.Phase != v1alpha1.PhaseTodo {
		t.Fatalf("deny: %+v, %v", out, err)
	}
	if _, err := c.Approve(ctx, ns, "open-pr", "alice", true, ""); !client.IsCode(err, httpjson.CodeInvalidTransition) {
		t.Fatalf("approve after the deny: %v", err)
	}
}
