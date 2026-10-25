package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
)

// cadre runs the CLI in-process and returns what it printed.
func cadre(t *testing.T, server string, args ...string) string {
	t.Helper()
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs(append([]string{"--server", server, "-n", "team-a", "--as", "alice"}, args...))
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("cadre %s: %v", strings.Join(args, " "), err)
	}
	return out.String()
}

func cadreFails(t *testing.T, server string, args ...string) error {
	t.Helper()
	root := newRoot()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs(append([]string{"--server", server, "-n", "team-a", "--as", "alice"}, args...))
	err := root.ExecuteContext(context.Background())
	if err == nil {
		t.Fatalf("cadre %s succeeded", strings.Join(args, " "))
	}
	return err
}

func mustContain(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Fatalf("output lacks %q:\n%s", w, got)
		}
	}
}

// A task goes through its whole life from the CLI: create, claim by the fake
// runner, a question for a human, the answer, a second run that finishes
// with subtasks, and the board showing all of it.
func TestTaskLifeFromTheCLI(t *testing.T) {
	m := manager.New(memory.NewBackend().Open(), manager.Config{Agents: manager.ReadyBuiltin("team-a", "coder", "claude")})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	api.NewServer(m, log).Register(mux)
	runner.NewServer(m, log, "http://cadre.test").Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx := context.Background()
	settle := func() {
		if err := m.Dispatch(ctx); err != nil {
			t.Fatal(err)
		}
		if err := m.NumberNew(ctx); err != nil {
			t.Fatal(err)
		}
	}

	mustContain(t, cadre(t, srv.URL, "team", "init", "--max-concurrent-runs", "2", "--role", "admin=user:lead@example.test"), "team team-a ready, 2 concurrent runs")
	// set changes what it is told to, and its roles replace the Team's.
	mustContain(t, cadre(t, srv.URL, "team", "set", "--display-name", "Team A", "--archive-after", "720h",
		"--role", "admin=user:lead@example.test", "--role", "member=group:backend,user:dev@example.test", "--role", "admin=group:platform"),
		"team team-a updated: 2 concurrent runs, 2 roles")
	team, err := m.LiveTeam(ctx, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if s := team.Spec; s.DisplayName != "Team A" || s.MaxConcurrentRuns != 2 || s.ArchiveAfter.Duration != 720*time.Hour ||
		len(s.Roles) != 2 || s.Roles[0].Role != "admin" || len(s.Roles[0].Subjects) != 2 || len(s.Roles[1].Subjects) != 2 {
		t.Fatalf("team spec %+v", s)
	}
	// 0 clears archiving; the run cap, not given, stays.
	mustContain(t, cadre(t, srv.URL, "team", "set", "--archive-after", "0"), "team team-a updated: 2 concurrent runs, 2 roles")
	if team, err := m.LiveTeam(ctx, "team-a"); err != nil || team.Spec.ArchiveAfter != nil || team.Spec.MaxConcurrentRuns != 2 {
		t.Fatalf("after --archive-after 0: %+v, %v", team, err)
	}
	if err := cadreFails(t, srv.URL, "team", "set", "--max-concurrent-runs", "0"); !strings.Contains(err.Error(), "at least 1") {
		t.Fatalf("a run cap of 0: %v", err)
	}
	if err := cadreFails(t, srv.URL, "team", "set", "--role", "owner"); !strings.Contains(err.Error(), "want role=") {
		t.Fatalf("a role without subjects: %v", err)
	}
	if err := cadreFails(t, srv.URL, "team", "set", "--role", "owner=user:x"); !strings.Contains(err.Error(), "not admin, member or viewer") {
		t.Fatalf("a role that does not exist: %v", err)
	}
	mustContain(t, cadre(t, srv.URL, "task", "create", "Fix the flaky login test", "--name", "fix-login", "--agent", "coder", "-p", "high"), "created #1 fix-login")
	mustContain(t, cadre(t, srv.URL, "task", "comment", "#1", "-m", "it fails on Mondays"), "comment 1 on fix-login")

	cadre(t, srv.URL, "runner", "fake", "--once", "--work", "10ms", "--runner", "fake-1", "--outcome", "Waiting", "--label", "cadre.dev/runtime=builtin")
	settle()
	mustContain(t, cadre(t, srv.URL, "task", "get", "#1"), "Phase:     Waiting", "Waiting:   Question", "alice (Comment): it fails on Mondays", "Finished", "fake-1")

	mustContain(t, cadre(t, srv.URL, "task", "reply", "fix-login", "-m", "use the staging database"), "#1 is Todo")
	if err := cadreFails(t, srv.URL, "task", "reply", "fix-login", "-m", "again"); !strings.Contains(err.Error(), "not waiting") {
		t.Fatalf("reply to a Todo task: %v", err)
	}

	cadre(t, srv.URL, "runner", "fake", "--once", "--work", "10ms", "--subtasks", "2", "-l", "cadre.dev/runtime=builtin")
	settle()
	list := cadre(t, srv.URL, "task", "list")
	mustContain(t, list, "NUMBER", "#1", "fix-login", "Done", "Subtask 1 of Fix the flaky login test", "Subtask 2 of Fix the flaky login test")
	if got := strings.Count(list, "Todo"); got != 2 {
		t.Fatalf("%d Todo subtasks, want 2:\n%s", got, list)
	}
	mustContain(t, cadre(t, srv.URL, "task", "list", "--phase", "Done"), "fix-login")
	// --parent on create takes a number too.
	mustContain(t, cadre(t, srv.URL, "task", "create", "Write the postmortem", "--name", "postmortem", "--parent", "#1"), "postmortem")
	for _, parent := range []string{"fix-login", "#1"} {
		subtasks := cadre(t, srv.URL, "task", "list", "--parent", parent)
		mustContain(t, subtasks, "Subtask 1 of Fix the flaky login test", "Subtask 2 of Fix the flaky login test", "Write the postmortem")
		if lines := strings.Count(strings.TrimSpace(subtasks), "\n") + 1; lines != 4 || strings.Contains(subtasks, "#1 ") {
			t.Fatalf("subtasks of %s:\n%s", parent, subtasks)
		}
	}

	mustContain(t, cadre(t, srv.URL, "task", "cancel", "#2"), "#2 is Cancelled")
	if err := cadreFails(t, srv.URL, "task", "get", "#99"); !strings.Contains(err.Error(), "no task #99") {
		t.Fatalf("get #99: %v", err)
	}
	if err := cadreFails(t, srv.URL, "task", "get", "#0"); !strings.Contains(err.Error(), "not a task number") {
		t.Fatalf("get #0: %v", err)
	}
}

// CADRE_NAMESPACE limits the fake runner as -n does.
func TestRunnerFakeHonoursCadreNamespace(t *testing.T) {
	// The handler may still be running when the command returns, so the
	// first claim is handed over through a channel, not a shared variable.
	claims := make(chan runner.ClaimRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req runner.ClaimRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		select {
		case claims <- req:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("CADRE_NAMESPACE", "team-b")
	root := newRoot()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--server", srv.URL, "runner", "fake", "--poll", "10ms"})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = root.ExecuteContext(ctx)
	got := <-claims
	if len(got.Namespaces) != 1 || got.Namespaces[0] != "team-b" {
		t.Fatalf("claimed in %v, want only team-b", got.Namespaces)
	}
}

// approver is a ToolCaller that echoes the call.
type approver struct{}

func (approver) CallTool(_ context.Context, _ string, call manager.ToolCall) manager.ToolResult {
	return manager.ToolResult{Body: "opened " + string(call.Arguments)}
}

func (approver) CallTimeout() time.Duration { return time.Minute }

func TestApproveAndDenyFromTheCLI(t *testing.T) {
	m := manager.New(memory.NewBackend().Open(), manager.Config{Agents: manager.ReadyBuiltin("team-a", "coder", "claude"), Tools: approver{}})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	api.NewServer(m, log).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx := context.Background()
	key := storage.Key{Namespace: "team-a", Name: "open-pr"}
	ask := func(args string) {
		t.Helper()
		c, err := m.Claim(ctx, manager.ClaimRequest{Runner: "r", Labels: map[string]string{v1alpha1.LabelRuntime: string(v1alpha1.RuntimeBuiltin)}})
		if err != nil || c == nil {
			t.Fatalf("claim: %+v, %v", c, err)
		}
		call := manager.ToolCall{Name: "github__create_pull_request", ToolServer: "github", Tool: "create_pull_request", Arguments: []byte(args)}
		if _, err := m.RunToolCall(ctx, key, c.RunID, call); err != nil {
			t.Fatal(err)
		}
	}

	cadre(t, srv.URL, "team", "init")
	cadre(t, srv.URL, "task", "create", "Open a PR", "--name", "open-pr", "--agent", "coder")
	if err := cadreFails(t, srv.URL, "task", "approve", "open-pr"); !strings.Contains(err.Error(), "not waiting for a tool approval") {
		t.Fatalf("approve a Todo task: %v", err)
	}
	ask(`{"title":"fix login"}`)
	mustContain(t, cadre(t, srv.URL, "task", "get", "open-pr"), "Waiting:   ToolApproval", "(ToolCall): Calls github__create_pull_request", `arguments: {"title":"fix login"}`)
	if err := cadreFails(t, srv.URL, "task", "reply", "open-pr", "-m", "yes"); !strings.Contains(err.Error(), "use approve or deny") {
		t.Fatalf("reply to a tool approval: %v", err)
	}
	mustContain(t, cadre(t, srv.URL, "task", "approve", "open-pr"), `The call returned: opened {"title":"fix login"}`, "#1 is Todo")
	ask(`{}`)
	mustContain(t, cadre(t, srv.URL, "task", "deny", "#1", "-m", "not today"), "#1 is Todo")
	mustContain(t, cadre(t, srv.URL, "task", "get", "#1"), "alice (Denied): not today")
}

// failSecondStatusWrite fails the second task status write, without effect.
type failSecondStatusWrite struct {
	storage.Driver
	n int
}

func (f *failSecondStatusWrite) UpdateTaskStatus(c context.Context, t *v1alpha1.Task) error {
	if f.n++; f.n == 2 {
		return errors.New("timeout")
	}
	return f.Driver.UpdateTaskStatus(c, t)
}

// Approving a call that is committed but not in the thread yet says to try
// again, as replying to a question does.
func TestApproveWhileTheCallIsRecorded(t *testing.T) {
	store := memory.NewBackend().Open()
	cfg := manager.Config{Agents: manager.ReadyBuiltin("team-a", "coder", "claude"), Tools: approver{}}
	m := manager.New(store, cfg)
	mux := http.NewServeMux()
	api.NewServer(m, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx := context.Background()
	cadre(t, srv.URL, "team", "init")
	cadre(t, srv.URL, "task", "create", "Open a PR", "--name", "open-pr", "--agent", "coder")
	c, err := m.Claim(ctx, manager.ClaimRequest{Runner: "r", Labels: map[string]string{v1alpha1.LabelRuntime: string(v1alpha1.RuntimeBuiltin)}})
	if err != nil || c == nil {
		t.Fatalf("claim: %+v, %v", c, err)
	}
	// The Finish lands; writing the ToolCall entry does not, yet.
	call := manager.ToolCall{Name: "github__create_pull_request", ToolServer: "github", Tool: "create_pull_request", Arguments: []byte(`{}`)}
	if _, err := manager.New(&failSecondStatusWrite{Driver: store}, cfg).RunToolCall(ctx, storage.Key{Namespace: "team-a", Name: "open-pr"}, c.RunID, call); err != nil {
		t.Fatal(err)
	}
	if err := cadreFails(t, srv.URL, "task", "approve", "open-pr"); !strings.Contains(err.Error(), "still being recorded") {
		t.Fatalf("approve a call still being recorded: %v", err)
	}
	if err := m.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	mustContain(t, cadre(t, srv.URL, "task", "approve", "open-pr"), "The call returned: opened {}")
}

// In a Job the fake claims under the Job's name, the Job's agent's tasks
// only, and exits after a claim that finds no work.
func TestRunnerFakeInAJob(t *testing.T) {
	claims := make(chan runner.ClaimRequest, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req runner.ClaimRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		claims <- req
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("CADRE_SERVER", srv.URL)
	t.Setenv("CADRE_NAMESPACE", "team-a")
	t.Setenv("CADRE_RUNNER", "coder-01jb")
	t.Setenv("CADRE_AGENT", "coder")
	root := newRoot()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"runner", "fake", "--poll", "10ms"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second || len(claims) != 1 {
		t.Fatalf("%d claims in %s; want one, then exit", len(claims), time.Since(start))
	}
	got := <-claims
	if got.Runner != "coder-01jb" || got.Agent != "coder" || len(got.Namespaces) != 1 || got.Namespaces[0] != "team-a" {
		t.Fatalf("claim %+v", got)
	}
}

// #N is the server's lookup by number: one list by number, never a scan of
// the board's pages.
func TestNumberIsTheServersLookup(t *testing.T) {
	var lists []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/team-a/tasks" {
			http.NotFound(w, r)
			return
		}
		lists = append(lists, r.URL.RawQuery)
		items := []v1alpha1.Task{}
		if r.URL.Query().Get("number") == "7" {
			tk := v1alpha1.Task{}
			tk.Name, tk.Status.Number = "seven", 7
			items = append(items, tk)
		}
		httpjson.Write(w, http.StatusOK, api.TaskList{Items: items, Limit: api.DefaultLimit})
	}))
	defer srv.Close()
	c := client.New(srv.URL, nil)
	if name, err := resolve(context.Background(), c, "team-a", "#7"); err != nil || name != "seven" {
		t.Fatalf("#7: %q, %v", name, err)
	}
	if _, err := resolve(context.Background(), c, "team-a", "#8"); err == nil || !strings.Contains(err.Error(), "no task #8") {
		t.Fatalf("#8: %v", err)
	}
	if len(lists) != 2 || lists[0] != "number=7" || lists[1] != "number=8" {
		t.Fatalf("lists %q", lists)
	}
}
