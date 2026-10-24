//go:build smoke

package anthropic_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/builtin"
	cadreclient "github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/mcp"
	"github.com/j75689/cadre/internal/provider/anthropic"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
	"github.com/j75689/cadre/internal/testenv"
)

// envSecret hands the run the smoke key, as a ModelProvider's Secret would.
type envSecret string

func (s envSecret) Value(context.Context, string, v1alpha1.SecretKeyRef) (string, error) {
	return string(s), nil
}

// smokeServer serves agents in ns, their model the real Anthropic API,
// with runners authenticated as cadre-runner's ServiceAccount by a real
// TokenReview; it returns the manager, the server's URL, the runner's
// token file and the log.
func smokeServer(t *testing.T, ns string, agents manager.StaticAgents, cfg manager.Config) (*manager.Manager, string, string, *slog.Logger) {
	t.Helper()
	agents.ModelProviders[ns+"/claude"].Spec.Auth = &v1alpha1.Auth{SecretRef: v1alpha1.SecretKeyRef{Name: "anthropic", Key: "api-key"}}
	cfg.Agents = agents
	m := manager.New(memory.NewBackend().Open(), cfg)

	// The runner authenticates as cadre-runner's ServiceAccount, with a
	// real token for the audience cadre, reviewed by a real API server.
	rest := testenv.APIServer(t)
	kube, err := client.New(rest, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cs := kubernetes.NewForConfigOrDie(rest)
	if _, err := cs.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cadre-system"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().ServiceAccounts("cadre-system").Create(context.Background(), &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "cadre-runner"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	tr, err := cs.CoreV1().ServiceAccounts("cadre-system").CreateToken(context.Background(), "cadre-runner",
		&authnv1.TokenRequest{Spec: authnv1.TokenRequestSpec{Audiences: []string{controller.RunnerAudience}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(tr.Status.Token), 0o600); err != nil {
		t.Fatal(err)
	}
	const builtinSA = "system:serviceaccount:cadre-system:cadre-runner"
	auth := controller.Runners(kube, runner.Cached(controller.TokenReviews(kube)), builtinSA)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	runner.NewServer(m, log, srv.URL, runner.WithAuth(auth, builtinSA)).Register(mux)
	mcp.NewServer(m, log).Register(mux)
	if _, err := m.InitTeam(context.Background(), ns, v1alpha1.TeamSpec{}); err != nil {
		t.Fatal(err)
	}
	return m, srv.URL, tokenFile, log
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
	// Run whole, so runtime mode stays covered live now that step is the
	// default.
	agents.Agents[ns+"/greeter"].Spec.Runtime.Execution = v1alpha1.RunModeRuntime
	agents.Agents[ns+"/greeter"].Spec.Instructions = "Call thread_post exactly once with a one-line greeting. Then call thread_ask exactly once, asking whether to continue. Do nothing else."
	m, url, tokenFile, log := smokeServer(t, ns, agents, manager.Config{})
	ctx := context.Background()
	if _, err := m.CreateTask(ctx, ns, "hello", v1alpha1.TaskSpec{Title: "Say hello, then ask", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "greeter"}}); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	hc, _ := cadreclient.RunnerHTTP(tokenFile)
	err := builtin.Run(runCtx, cadreclient.New(url, hc), builtin.Options{
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

// messages counts the requests to the Messages API.
type messages struct {
	mu sync.Mutex
	n  int
}

func (c *messages) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/messages") {
		c.mu.Lock()
		c.n++
		c.mu.Unlock()
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (c *messages) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// dying is a worker's connection to cadre-server that goes dead once the
// worker has written a turn's entry: the step is neither committed nor
// given back, as when the worker dies there.
type dying struct {
	next        http.RoundTripper
	mu          sync.Mutex
	armed, died bool
}

func (d *dying) RoundTrip(r *http.Request) (*http.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case r.URL.Path == runner.PathStepAppend && r.Body != nil:
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		d.armed = d.armed || bytes.Contains(body, []byte(`"kind":"llm_turn"`))
	case d.armed && (r.URL.Path == runner.PathStepAdvance || r.URL.Path == runner.PathStepRelease):
		d.died = true
		return &http.Response{StatusCode: http.StatusConflict, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"error":{"code":"attempt_mismatch","message":"gone"}}`)), Request: r}, nil
	}
	return d.next.RoundTrip(r)
}

func (d *dying) gone() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.died
}

// TestSmokeStepMode runs a step-mode run against the real Anthropic API,
// each step on the next of two workers, one of which dies after writing a
// turn's entry: the other commits that entry without calling the model
// again. It runs only with `make smoke`.
func TestSmokeStepMode(t *testing.T) {
	apiKey := os.Getenv("CADRE_SMOKE_ANTHROPIC_KEY")
	if apiKey == "" {
		t.Skip("CADRE_SMOKE_ANTHROPIC_KEY is not set")
	}
	const ns = "smoke"
	agents := manager.ReadyBuiltin(ns, "greeter", "claude")
	agent := agents.Agents[ns+"/greeter"]
	agent.Spec.Runtime.Execution = v1alpha1.RunModeStep
	agent.Spec.Instructions = "Call thread_post exactly once with a one-line greeting. Then reply with a one-line summary and call no tool."
	m, url, tokenFile, log := smokeServer(t, ns, agents, manager.Config{Lease: 3 * time.Second, Heartbeat: time.Second})
	ctx := context.Background()
	key := storage.Key{Namespace: ns, Name: "hello"}
	if _, err := m.CreateTask(ctx, ns, key.Name, v1alpha1.TaskSpec{Title: "Say hello", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "greeter"}}); err != nil {
		t.Fatal(err)
	}

	api := &messages{}
	hc, _ := cadreclient.RunnerHTTP(tokenFile)
	c := cadreclient.New(url, hc)
	// The worker of the first turn dies once its entry is written.
	d := &dying{next: hc.Transport}
	dead := cadreclient.New(url, &http.Client{Transport: d})
	o := builtin.Options{
		Once: true, Poll: 10 * time.Millisecond, Secrets: envSecret(apiKey), Log: log, MaxTurns: 6,
		Providers: map[v1alpha1.ProviderType]builtin.Provider{
			v1alpha1.ProviderAnthropic: &anthropic.Provider{Log: log, Endpoint: "https://api.anthropic.com", HTTPClient: &http.Client{Transport: api}},
		},
	}
	workers := map[string]bool{}
	for i := 0; i < 20; i++ {
		tk, err := m.GetTask(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && tk.Status.Phase != v1alpha1.PhaseRunning {
			break
		}
		if r := tk.Status.Run; r != nil && !r.StepFree && d.gone() {
			// The dead worker's step: wait out its lease and grace, until
			// the reclaim frees it.
			for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(500 * time.Millisecond) {
				if err := m.ReclaimExpired(ctx); err != nil {
					t.Fatal(err)
				}
				if tk, err := m.GetTask(ctx, key); err == nil && (tk.Status.Run == nil || tk.Status.Run.StepFree) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the dead worker's step was never freed")
				}
			}
		}
		o.Runner = []string{"smoke-a", "smoke-b"}[i%2]
		workers[o.Runner] = true
		stepCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		worker := c
		if !d.gone() {
			worker = dead
		}
		err = builtin.RunSteps(stepCtx, worker, o)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}

	tk, err := m.GetTask(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	thread, _ := m.ReadThread(ctx, key, 0, 0)
	for _, e := range thread {
		t.Logf("thread %d %s %s: %s", e.Seq, e.Kind, e.Author.Name, e.Body)
	}
	if tk.Status.Phase != v1alpha1.PhaseReview || !d.gone() || len(workers) != 2 {
		t.Fatalf("phase %s, died %v, workers %v; want Review after a death, on two workers", tk.Status.Phase, d.gone(), workers)
	}
	entries, err := m.Transcript(ctx, key, tk.Status.Runs.LastID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	turns := 0
	for _, e := range entries {
		kinds = append(kinds, string(e.Kind))
		if e.Kind == manager.StepLLMTurn {
			turns++
		}
	}
	t.Logf("transcript: %s; %d Messages API requests", strings.Join(kinds, ", "), api.count())
	if len(kinds) < 4 || kinds[0] != "start" || kinds[1] != "llm_turn" || kinds[2] != "exec_tools" || kinds[len(kinds)-1] != "llm_turn" {
		t.Fatalf("transcript kinds %v", kinds)
	}
	if api.count() != turns {
		t.Fatalf("%d Messages API requests for %d turns; the adopted turn called the model again", api.count(), turns)
	}
	var posts int
	for _, e := range thread {
		if e.Author.Kind == manager.AuthorAgent && e.Kind == manager.EntryComment {
			posts++
		}
	}
	if posts != 2 { // the greeting and the summary
		t.Fatalf("%d comments by the agent, want the greeting and the summary", posts)
	}
}
