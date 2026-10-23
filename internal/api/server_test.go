package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/runner"
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
	m := manager.New(store, manager.Config{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	api.NewServer(m, log).Register(mux)
	runner.NewServer(m, log).Register(mux)
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
	if _, err := c.InitTeam(ctx, ns, api.TeamRequest{DisplayName: "Backend", MaxConcurrentRuns: 2}); err != nil {
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
	list, err := c.ListTasks(ctx, ns, nil, 0, 0)
	if err != nil || len(list.Items) != 2 || list.Items[0].Name != "fix-login" {
		t.Fatalf("list: %+v, %v", list, err)
	}
	if page, _ := c.ListTasks(ctx, ns, []v1alpha1.Phase{v1alpha1.PhaseRunning}, 0, 0); len(page.Items) != 0 {
		t.Fatalf("Running tasks: %+v", page.Items)
	}

	if err := c.Comment(ctx, ns, "fix-login", "alice", "it fails on Mondays"); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if _, err := c.Reply(ctx, ns, "fix-login", "alice", "not waiting"); !client.IsCode(err, httpjson.CodeInvalidTransition) {
		t.Fatalf("reply to a Todo task: %v", err)
	}

	claim, err := c.Claim(ctx, runner.ClaimRequest{Runner: "r"})
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
	if err := c.Comment(ctx, ns, "t", "", "x"); !client.IsCode(err, httpjson.CodeInvalid) {
		t.Errorf("comment without an author: %v", err)
	}
	if _, err := c.Events(ctx, ns, "t", "not-a-ulid"); !client.IsCode(err, httpjson.CodeInvalid) {
		t.Errorf("events for a bad run id: %v", err)
	}
	if _, err := c.InitTeam(ctx, "team-b", api.TeamRequest{MaxConcurrentRuns: -1}); !client.IsCode(err, httpjson.CodeInvalid) {
		t.Errorf("negative maxConcurrentRuns: %v", err)
	}
	resp, err := http.Get(base + "/api/v1/namespaces/" + ns + "/tasks?limit=-1")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("negative limit: status %d", resp.StatusCode)
	}
}
