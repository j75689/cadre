package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/builtin"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/mcp"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
)

const ns = "team-a"

var (
	ctx = context.Background()
	key = storage.Key{Namespace: ns, Name: "t"}
)

// step is one scripted model turn: it sees the results of the previous
// turn's calls and returns the reply.
type step func(ctx context.Context, results []builtin.ToolResult) (*builtin.Reply, error)

// scripted is a Provider whose model follows a script.
type scripted struct {
	mu      sync.Mutex
	steps   []step
	limit   int64
	setup   builtin.Setup
	results [][]builtin.ToolResult
}

func (p *scripted) Start(_ context.Context, s builtin.Setup) (builtin.Conversation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setup = s
	return &conversation{p: p}, nil
}

type conversation struct {
	p    *scripted
	turn int
	last []builtin.ToolResult
}

func (c *conversation) Turn(ctx context.Context) (*builtin.Reply, error) {
	c.p.mu.Lock()
	if c.turn >= len(c.p.steps) {
		c.p.mu.Unlock()
		return &builtin.Reply{Stop: builtin.StopEndTurn, Text: "out of script"}, nil
	}
	s := c.p.steps[c.turn]
	c.p.mu.Unlock()
	reply, err := s(ctx, c.last)
	if err == nil {
		c.turn++
	}
	return reply, err
}

func (c *conversation) AddResults(results []builtin.ToolResult) {
	c.last = results
	c.p.mu.Lock()
	c.p.results = append(c.p.results, results)
	c.p.mu.Unlock()
}

func (c *conversation) ContextLimit() int64 { return c.p.limit }

func calls(cs ...builtin.ToolCall) step {
	return func(context.Context, []builtin.ToolResult) (*builtin.Reply, error) {
		return &builtin.Reply{Stop: builtin.StopToolUse, Calls: cs, Usage: runner.Usage{InputTokens: 100, OutputTokens: 10, CacheReadInputTokens: 80}, ContextTokens: 1000}, nil
	}
}

func say(text string) step {
	return func(context.Context, []builtin.ToolResult) (*builtin.Reply, error) {
		return &builtin.Reply{Stop: builtin.StopEndTurn, Text: text}, nil
	}
}

func tc(id, name string, input any) builtin.ToolCall {
	data, _ := json.Marshal(input)
	return builtin.ToolCall{ID: id, Name: name, Input: data}
}

type env struct {
	m *manager.Manager
	c *client.Client
}

func start(t *testing.T) *env {
	t.Helper()
	return startWithLease(t, 2*time.Second)
}

// startWithLease runs the server with heartbeats every second and the given
// lease.
func startWithLease(t *testing.T, lease time.Duration) *env {
	t.Helper()
	m := manager.New(memory.NewBackend().Open(), manager.Config{
		Agents: manager.ReadyBuiltin(ns, "coder", "claude"), Lease: lease, Heartbeat: time.Second,
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	runner.NewServer(m, log, srv.URL).Register(mux)
	mcp.NewServer(m, log).Register(mux)
	if _, err := m.InitTeam(ctx, ns, v1alpha1.TeamSpec{MaxConcurrentRuns: 1}); err != nil {
		t.Fatal(err)
	}
	spec := v1alpha1.TaskSpec{Title: "Fix the flaky login test", Description: "It fails on Mondays.", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}}
	if _, err := m.CreateTask(ctx, ns, "t", spec); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendThread(ctx, key, manager.Author{Kind: manager.AuthorUser, Name: "alice"}, manager.EntryComment, "Look at the clock mock."); err != nil {
		t.Fatal(err)
	}
	return &env{m: m, c: client.New(srv.URL, nil)}
}

// run works one task with the scripted model.
func (e *env) run(t *testing.T, p builtin.Provider, o builtin.Options) {
	t.Helper()
	o.Runner, o.Once, o.Poll = "r", true, 10*time.Millisecond
	if o.Providers == nil {
		o.Providers = map[v1alpha1.ProviderType]builtin.Provider{v1alpha1.ProviderAnthropic: p}
	}
	if o.RetryFor == 0 {
		o.RetryFor = 3 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := builtin.Run(runCtx, e.c, o); err != nil {
		t.Fatal(err)
	}
}

func (e *env) task(t *testing.T) *v1alpha1.Task {
	t.Helper()
	tk, err := e.m.GetTask(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func (e *env) thread(t *testing.T) []manager.ThreadEntry {
	t.Helper()
	entries, err := e.m.ReadThread(ctx, key, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func last(entries []manager.ThreadEntry) manager.ThreadEntry { return entries[len(entries)-1] }

func TestARunWorksTheTaskAndGoesToReview(t *testing.T) {
	e := start(t)
	p := &scripted{steps: []step{
		calls(tc("1", "thread_post", map[string]any{"body": "Reading the test."}), tc("2", "task_files_write", map[string]any{"path": "notes.md", "content": "clock"})),
		calls(tc("3", "no_such_tool", map[string]any{})),
		say("Fixed the clock mock."),
	}}
	e.run(t, p, builtin.Options{})

	if tk := e.task(t); tk.Status.Phase != v1alpha1.PhaseReview {
		t.Fatalf("phase %s, want Review", tk.Status.Phase)
	}
	th := e.thread(t)
	if len(th) != 3 || th[1].Body != "Reading the test." || last(th).Body != "Fixed the clock mock." || last(th).Author.Kind != manager.AuthorAgent {
		t.Fatalf("thread: %+v", th)
	}
	// The model saw the task, the thread and the tools, sorted.
	s := p.setup
	for _, want := range []string{"# Task #1: Fix the flaky login test", "It fails on Mondays.", "user alice", "Look at the clock mock.", "no files yet"} {
		if !strings.Contains(s.Prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, s.Prompt)
		}
	}
	if !strings.Contains(s.System, "thread_ask") || len(s.Tools) != 7 || s.Tools[0].Name != "task_create_subtask" || s.Model.Model != "claude-opus-5-5" {
		t.Fatalf("setup: system %q, %d tools, model %+v", s.System, len(s.Tools), s.Model)
	}
	// Results come back in the order of the calls, one batch per turn,
	// and an unknown tool is an error result for the model.
	if len(p.results) != 2 || len(p.results[0]) != 2 || p.results[0][0].CallID != "1" || p.results[0][1].CallID != "2" || p.results[0][0].IsError ||
		!p.results[1][0].IsError {
		t.Fatalf("results: %+v", p.results)
	}
	runs, _ := e.m.Runs(ctx, key)
	events, err := e.m.RunEvents(ctx, key, runs[0].ID)
	if err != nil || len(events) != 3 || events[0].InputTokens != 100 || events[0].CacheReadInputTokens != 80 || events[0].ContextTokens != 1000 ||
		events[0].Progress != "turn 1: thread_post, task_files_write" {
		t.Fatalf("events: %+v, %v", events, err)
	}
}

// thread_ask ends the run Cadre-side: the runner makes no further call and
// does not finish, and a later call in the same turn is not made.
func TestAskStopsTheRunWithoutFinishing(t *testing.T) {
	e := start(t)
	p := &scripted{steps: []step{
		calls(tc("1", "thread_ask", map[string]any{"question": "Staging or production?"}), tc("2", "thread_post", map[string]any{"body": "never posted"})),
		say("never reached"),
	}}
	e.run(t, p, builtin.Options{})

	tk := e.task(t)
	if tk.Status.Phase != v1alpha1.PhaseWaiting || tk.Status.Waiting.Reason != v1alpha1.WaitingQuestion {
		t.Fatalf("after the ask: %+v", tk.Status)
	}
	if th := e.thread(t); last(th).Kind != manager.EntryQuestion || len(p.results) != 0 {
		t.Fatalf("thread %+v, results %+v", th, p.results)
	}
}

func TestEndingsThatNeedAHuman(t *testing.T) {
	refusal := func(context.Context, []builtin.ToolResult) (*builtin.Reply, error) {
		return &builtin.Reply{Stop: builtin.StopRefusal, RefusalCategory: "cyber"}, nil
	}
	cutOff := func(context.Context, []builtin.ToolResult) (*builtin.Reply, error) {
		return &builtin.Reply{Stop: builtin.StopMaxTokens}, nil
	}
	permanent := func(context.Context, []builtin.ToolResult) (*builtin.Reply, error) {
		return nil, builtin.Permanent(errors.New("invalid x-api-key"))
	}
	post := tc("1", "thread_post", map[string]any{"body": "working"})
	for _, tcase := range []struct {
		name    string
		p       *scripted
		o       builtin.Options
		phase   v1alpha1.Phase
		comment string
	}{
		{"refusal", &scripted{steps: []step{refusal}}, builtin.Options{}, v1alpha1.PhaseFailed, "declined to continue (category: cyber)"},
		{"max tokens", &scripted{steps: []step{cutOff}}, builtin.Options{}, v1alpha1.PhaseFailed, "cut off"},
		{"permanent error", &scripted{steps: []step{permanent}}, builtin.Options{}, v1alpha1.PhaseFailed, "The model call failed: invalid x-api-key"},
		{"max turns", &scripted{steps: []step{calls(post), calls(post), calls(post)}}, builtin.Options{MaxTurns: 2}, v1alpha1.PhaseReview, "after 2 model turns"},
		{"context ceiling", &scripted{steps: []step{calls(post), say("never")}, limit: 1200}, builtin.Options{}, v1alpha1.PhaseReview, "1000 of the model's 1200 context tokens"},
		{"unsupported provider", &scripted{}, builtin.Options{Providers: map[v1alpha1.ProviderType]builtin.Provider{}}, v1alpha1.PhaseFailed, `type "anthropic"`},
	} {
		t.Run(tcase.name, func(t *testing.T) {
			e := start(t)
			e.run(t, tcase.p, tcase.o)
			if tk := e.task(t); tk.Status.Phase != tcase.phase {
				t.Fatalf("phase %s, want %s", tk.Status.Phase, tcase.phase)
			}
			if got := last(e.thread(t)).Body; !strings.Contains(got, tcase.comment) {
				t.Fatalf("last comment %q, want it to say %q", got, tcase.comment)
			}
		})
	}
}

// A model call that fails for a while is retried; one that keeps failing
// past RetryFor fails the run.
func TestTransientModelErrorsAreRetried(t *testing.T) {
	failures := 2
	flaky := func(context.Context, []builtin.ToolResult) (*builtin.Reply, error) {
		if failures > 0 {
			failures--
			return nil, errors.New("overloaded")
		}
		return &builtin.Reply{Stop: builtin.StopEndTurn, Text: "done after retries"}, nil
	}
	e := start(t)
	e.run(t, &scripted{steps: []step{flaky}}, builtin.Options{RetryFor: 10 * time.Second})
	if tk := e.task(t); tk.Status.Phase != v1alpha1.PhaseReview || last(e.thread(t)).Body != "done after retries" {
		t.Fatalf("after retries: %s, %q", tk.Status.Phase, last(e.thread(t)).Body)
	}

	down := func(context.Context, []builtin.ToolResult) (*builtin.Reply, error) {
		return nil, errors.New("overloaded")
	}
	e = start(t)
	e.run(t, &scripted{steps: []step{down}}, builtin.Options{RetryFor: 1500 * time.Millisecond})
	if tk := e.task(t); tk.Status.Phase != v1alpha1.PhaseFailed || !strings.Contains(last(e.thread(t)).Body, "overloaded") {
		t.Fatalf("after giving up: %s, %q", tk.Status.Phase, last(e.thread(t)).Body)
	}
}

// A task cancelled mid-run stops the run at its next heartbeat, long before
// its lease would run out; it does not finish.
func TestCancelStopsTheRun(t *testing.T) {
	e := startWithLease(t, 30*time.Second)
	started := make(chan struct{})
	hang := func(ctx context.Context, _ []builtin.ToolResult) (*builtin.Reply, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	go func() {
		<-started
		if _, err := e.m.Cancel(ctx, key); err != nil {
			t.Error(err)
		}
	}()
	begin := time.Now()
	e.run(t, &scripted{steps: []step{hang}}, builtin.Options{})
	if tk := e.task(t); tk.Status.Phase != v1alpha1.PhaseCancelled {
		t.Fatalf("phase %s, want Cancelled", tk.Status.Phase)
	}
	if took := time.Since(begin); took > 5*time.Second {
		t.Fatalf("the run took %s to notice the cancel", took)
	}
	if th := e.thread(t); len(th) != 1 {
		t.Fatalf("the cancelled run wrote to the thread: %+v", th)
	}
}

// The whole thread reaches the model, read a page at a time.
func TestTheWholeThreadIsRead(t *testing.T) {
	e := start(t)
	for i := range manager.MaxThreadPage + 50 {
		if _, err := e.m.AppendThread(ctx, key, manager.Author{Kind: manager.AuthorUser, Name: "alice"}, manager.EntryComment, fmt.Sprintf("note %d", i+2)); err != nil {
			t.Fatal(err)
		}
	}
	p := &scripted{steps: []step{say("done")}}
	e.run(t, p, builtin.Options{})
	for _, want := range []string{"Look at the clock mock.", "note 201", fmt.Sprintf("note %d", manager.MaxThreadPage+51)} {
		if !strings.Contains(p.setup.Prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}

// counting counts claim requests.
type counting struct {
	claims atomic.Int32
	next   http.RoundTripper
}

func (c *counting) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/claim") {
		c.claims.Add(1)
	}
	return c.next.RoundTrip(r)
}

// A run whose MCP endpoint cannot be reached is left to its lease, and the
// runner waits before claiming again, longer each time, so a dead endpoint
// does not become a loop of claims holding Team slots.
func TestUnreachableEndpointBacksOff(t *testing.T) {
	m := manager.New(memory.NewBackend().Open(), manager.Config{Agents: manager.ReadyBuiltin(ns, "coder", "claude")})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	runner.NewServer(m, log, srv.URL).Register(mux) // no MCP endpoint
	if _, err := m.InitTeam(ctx, ns, v1alpha1.TeamSpec{MaxConcurrentRuns: 3}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if _, err := m.CreateTask(ctx, ns, name, v1alpha1.TaskSpec{Title: name, Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}}); err != nil {
			t.Fatal(err)
		}
	}
	cnt := &counting{next: http.DefaultTransport}
	runCtx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()
	err := builtin.Run(runCtx, client.New(srv.URL, &http.Client{Transport: cnt}), builtin.Options{
		Runner: "r", Poll: 10 * time.Millisecond, Backoff: 500 * time.Millisecond,
		Providers: map[v1alpha1.ProviderType]builtin.Provider{v1alpha1.ProviderAnthropic: &scripted{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// One claim, 500ms, a second, 1s: the third task is never claimed.
	if n := cnt.claims.Load(); n != 2 {
		t.Fatalf("%d claims in 1.2s, want 2", n)
	}
	if tk, _ := m.GetTask(ctx, storage.Key{Namespace: ns, Name: "c"}); tk.Status.Phase != v1alpha1.PhaseTodo {
		t.Fatalf("the third task is %s, want Todo", tk.Status.Phase)
	}
}
