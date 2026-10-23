package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/runner"
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
	m := manager.New(memory.NewBackend().Open(), manager.Config{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	api.NewServer(m, log).Register(mux)
	runner.NewServer(m, log).Register(mux)
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

	mustContain(t, cadre(t, srv.URL, "team", "init", "--max-concurrent-runs", "2"), "team team-a ready, 2 concurrent runs")
	mustContain(t, cadre(t, srv.URL, "task", "create", "Fix the flaky login test", "--name", "fix-login", "--agent", "coder", "-p", "high"), "created #1 fix-login")
	mustContain(t, cadre(t, srv.URL, "task", "comment", "#1", "-m", "it fails on Mondays"), "comment 1 on fix-login")

	cadre(t, srv.URL, "runner", "fake", "--once", "--work", "10ms", "--runner", "fake-1", "--outcome", "Waiting")
	settle()
	mustContain(t, cadre(t, srv.URL, "task", "get", "#1"), "Phase:     Waiting", "Waiting:   Question", "alice (Comment): it fails on Mondays", "Finished", "fake-1")

	mustContain(t, cadre(t, srv.URL, "task", "reply", "fix-login", "-m", "use the staging database"), "#1 is Todo")
	if err := cadreFails(t, srv.URL, "task", "reply", "fix-login", "-m", "again"); !strings.Contains(err.Error(), "not waiting") {
		t.Fatalf("reply to a Todo task: %v", err)
	}

	cadre(t, srv.URL, "runner", "fake", "--once", "--work", "10ms", "--subtasks", "2")
	settle()
	list := cadre(t, srv.URL, "task", "list")
	mustContain(t, list, "NUMBER", "#1", "fix-login", "Done", "Subtask 1 of Fix the flaky login test", "Subtask 2 of Fix the flaky login test")
	if got := strings.Count(list, "Todo"); got != 2 {
		t.Fatalf("%d Todo subtasks, want 2:\n%s", got, list)
	}
	mustContain(t, cadre(t, srv.URL, "task", "list", "--phase", "Done"), "fix-login")

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
	var got runner.ClaimRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
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
	if len(got.Namespaces) != 1 || got.Namespaces[0] != "team-b" {
		t.Fatalf("claimed in %v, want only team-b", got.Namespaces)
	}
}
