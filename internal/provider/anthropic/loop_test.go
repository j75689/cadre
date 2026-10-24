package anthropic_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// secret is the credential the fake API wants, as a Secret holds it.
type secret struct{}

func (secret) Value(context.Context, string, v1alpha1.SecretKeyRef) (string, error) {
	return key + "\n", nil // written from a file
}

// The builtin loop on the Anthropic provider, against a Task Manager and a
// fake Messages API: the model posts, then sums up, and the task goes to
// Review.
func TestTheLoopOnTheAnthropicProvider(t *testing.T) {
	const ns = "team-a"
	agents := manager.ReadyBuiltin(ns, "coder", "claude")
	agents.ModelProviders[ns+"/claude"].Spec.Auth = &v1alpha1.Auth{SecretRef: v1alpha1.SecretKeyRef{Name: "anthropic", Key: "api-key"}}
	m := manager.New(memory.NewBackend().Open(), manager.Config{Agents: agents})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	runner.NewServer(m, log, srv.URL).Register(mux)
	mcp.NewServer(m, log).Register(mux)
	if _, err := m.InitTeam(ctx, ns, v1alpha1.TeamSpec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateTask(ctx, ns, "t", v1alpha1.TaskSpec{Title: "Say hi", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}}); err != nil {
		t.Fatal(err)
	}

	a := newAPI(t, sse("tool_use", "", [2]string{"thread_post", `{"body":"hi"}`}), sse("end_turn", "Said hi."))
	runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	err := builtin.Run(runCtx, client.New(srv.URL, nil), builtin.Options{
		Runner: "r", Once: true, Poll: 10 * time.Millisecond, Secrets: secret{},
		Providers: map[v1alpha1.ProviderType]builtin.Provider{v1alpha1.ProviderAnthropic: &anthropic.Provider{Endpoint: a.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := storage.Key{Namespace: ns, Name: "t"}
	tk, _ := m.GetTask(ctx, task)
	thread, _ := m.ReadThread(ctx, task, 0, 0)
	if tk.Status.Phase != v1alpha1.PhaseReview || len(thread) != 2 || thread[0].Body != "hi" || thread[1].Body != "Said hi." {
		t.Fatalf("phase %s, thread %+v", tk.Status.Phase, thread)
	}
	if got := a.headers[0].Get("X-Api-Key"); got != key {
		t.Fatalf("API key %q: the Secret's value, trimmed, is the credential", got)
	}
}
