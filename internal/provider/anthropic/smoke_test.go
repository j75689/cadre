//go:build smoke

package anthropic_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/builtin"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/mcp"
	"github.com/j75689/cadre/internal/provider/anthropic"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
)

// envSecret hands the run the smoke key, as a ModelProvider's Secret would.
type envSecret string

func (s envSecret) Value(context.Context, string, v1alpha1.SecretKeyRef) (string, error) {
	return string(s), nil
}

// TestSmoke drives the builtin loop against the real Anthropic API. It runs
// only with `make smoke` and CADRE_SMOKE_ANTHROPIC_KEY set; it costs a few
// cents.
func TestSmoke(t *testing.T) {
	apiKey := os.Getenv("CADRE_SMOKE_ANTHROPIC_KEY")
	if apiKey == "" {
		t.Skip("CADRE_SMOKE_ANTHROPIC_KEY is not set")
	}
	const ns = "smoke"
	agents := manager.ReadyBuiltin(ns, "greeter", "claude")
	agents.Agents[ns+"/greeter"].Spec.Instructions = "Call thread_post exactly once with a one-line greeting. Then call thread_ask exactly once, asking whether to continue. Do nothing else."
	agents.ModelProviders[ns+"/claude"].Spec.Auth = &v1alpha1.Auth{SecretRef: v1alpha1.SecretKeyRef{Name: "anthropic", Key: "api-key"}}
	m := manager.New(memory.NewBackend().Open(), manager.Config{Agents: agents})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	runner.NewServer(m, log, srv.URL).Register(mux)
	mcp.NewServer(m, log).Register(mux)
	ctx := context.Background()
	if _, err := m.InitTeam(ctx, ns, v1alpha1.TeamSpec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateTask(ctx, ns, "hello", v1alpha1.TaskSpec{Title: "Say hello, then ask", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "greeter"}}); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	err := builtin.Run(runCtx, client.New(srv.URL, nil), builtin.Options{
		Runner: "smoke", Once: true, Poll: 10 * time.Millisecond, Secrets: envSecret(apiKey), Log: log, MaxTurns: 6,
		Providers: map[v1alpha1.ProviderType]builtin.Provider{
			// Anthropic's own API, named outright whatever the environment
			// says; the provider ignores the environment anyway.
			v1alpha1.ProviderAnthropic: &anthropic.Provider{Log: log, Endpoint: "https://api.anthropic.com"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	key := storage.Key{Namespace: ns, Name: "hello"}
	tk, err := m.GetTask(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	thread, _ := m.ReadThread(ctx, key, 0, 0)
	for _, e := range thread {
		t.Logf("thread %d %s %s: %s", e.Seq, e.Kind, e.Author.Name, e.Body)
	}
	if tk.Status.Phase != v1alpha1.PhaseWaiting || tk.Status.Waiting.Reason != v1alpha1.WaitingQuestion {
		t.Fatalf("phase %s, waiting %+v; want Waiting{Question}", tk.Status.Phase, tk.Status.Waiting)
	}
	var comments, questions int
	for _, e := range thread {
		if e.Author.Kind != manager.AuthorAgent {
			continue
		}
		switch e.Kind {
		case manager.EntryComment:
			comments++
		case manager.EntryQuestion:
			questions++
		}
	}
	if comments != 1 || questions != 1 {
		t.Fatalf("%d comments and %d questions by the agent, want one of each", comments, questions)
	}
	runs, _ := m.Runs(ctx, key)
	events, err := m.RunEvents(ctx, key, runs[len(runs)-1].ID)
	if err != nil || len(events) == 0 {
		t.Fatalf("events: %+v, %v", events, err)
	}
	for _, ev := range events {
		t.Logf("event %d: input %d, output %d, cache read %d, cache creation %d, context %d, %s",
			ev.Seq, ev.InputTokens, ev.OutputTokens, ev.CacheReadInputTokens, ev.CacheCreationInputTokens, ev.ContextTokens, ev.Progress)
		if ev.InputTokens+ev.CacheReadInputTokens+ev.CacheCreationInputTokens == 0 {
			t.Errorf("event %d reports no input tokens", ev.Seq)
		}
	}
}
