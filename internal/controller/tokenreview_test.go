package controller_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	cadreclient "github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/httpjson"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
	"github.com/j75689/cadre/internal/testenv"
)

// bearer sends a fixed token.
type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b != "" {
		r.Header.Set("Authorization", "Bearer "+string(b))
	}
	return http.DefaultTransport.RoundTrip(r)
}

// Runners authenticate with real ServiceAccount tokens, reviewed by a real
// API server: only cadre-runner's ServiceAccount claims builtin agents, a
// token for another audience or none is refused, and only the runner
// holding a run may renew or finish it.
func TestRunnersAuthenticateWithServiceAccountTokens(t *testing.T) {
	cfg := testenv.APIServer(t)
	ctx := context.Background()
	cs := kubernetes.NewForConfigOrDie(cfg)
	kube, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	token := func(ns, name string, audiences ...string) string {
		t.Helper()
		_, _ = cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
		_, _ = cs.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
		tr, err := cs.CoreV1().ServiceAccounts(ns).CreateToken(ctx, name, &authnv1.TokenRequest{Spec: authnv1.TokenRequestSpec{Audiences: audiences}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return tr.Status.Token
	}
	builtinToken := token("cadre-system", "cadre-runner", controller.RunnerAudience)
	otherToken := token("team-a", "someone", controller.RunnerAudience)
	apiServerToken := token("cadre-system", "cadre-runner") // the API server's own audience

	const ns = "team-a"
	m := manager.New(memory.NewBackend().Open(), manager.Config{Agents: manager.ReadyBuiltin(ns, "coder", "claude")})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	runner.NewServer(m, log, "http://cadre.test",
		runner.WithAuth(runner.Cached(controller.TokenReviews(kube)), "system:serviceaccount:cadre-system:cadre-runner")).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if _, err := m.InitTeam(ctx, ns, v1alpha1.TeamSpec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateTask(ctx, ns, "t", v1alpha1.TaskSpec{Title: "t", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}}); err != nil {
		t.Fatal(err)
	}
	as := func(tok string) *cadreclient.Client {
		return cadreclient.New(srv.URL, &http.Client{Transport: bearer(tok)})
	}
	builtin := map[string]string{v1alpha1.LabelRuntime: string(v1alpha1.RuntimeBuiltin)}
	claim := runner.ClaimRequest{Runner: "pod-1", Labels: builtin}

	for name, tok := range map[string]string{"no token": "", "the API server's audience": apiServerToken, "garbage": "not-a-token"} {
		if _, err := as(tok).Claim(ctx, claim); !cadreclient.IsCode(err, httpjson.CodeUnauthenticated) {
			t.Errorf("claim with %s: %v, want unauthenticated", name, err)
		}
	}
	// Another ServiceAccount sending the builtin label claims nothing: the
	// label is reserved for cadre-runner.
	if c, err := as(otherToken).Claim(ctx, claim); err != nil || c != nil {
		t.Fatalf("a claim by another identity: %+v, %v; want no work", c, err)
	}
	c, err := as(builtinToken).Claim(ctx, claim)
	if err != nil || c == nil {
		t.Fatalf("cadre-runner's claim: %+v, %v", c, err)
	}
	tk, _ := m.GetTask(ctx, storage.Key{Namespace: ns, Name: "t"})
	if r := tk.Status.Run; r.Runner != "system:serviceaccount:cadre-system:cadre-runner" || r.RunnerName != "pod-1" {
		t.Fatalf("run held by %q named %q", r.Runner, r.RunnerName)
	}

	ref := runner.TaskRef{Namespace: ns, Name: "t"}
	if _, err := as(otherToken).Heartbeat(ctx, runner.HeartbeatRequest{Task: ref, RunID: c.RunID}); !cadreclient.IsCode(err, httpjson.CodeForbidden) {
		t.Fatalf("another identity's heartbeat with the right run id: %v, want forbidden", err)
	}
	if err := as(otherToken).Event(ctx, runner.EventsRequest{Task: ref, RunID: c.RunID, Seq: 1}); !cadreclient.IsCode(err, httpjson.CodeForbidden) {
		t.Fatalf("another identity's event: %v, want forbidden", err)
	}
	if _, err := as(otherToken).Finish(ctx, runner.FinishRequest{Task: ref, RunID: c.RunID, Outcome: v1alpha1.RunOutcomeDone}); !cadreclient.IsCode(err, httpjson.CodeForbidden) {
		t.Fatalf("another identity's finish: %v, want forbidden", err)
	}
	if _, err := as(builtinToken).Heartbeat(ctx, runner.HeartbeatRequest{Task: ref, RunID: c.RunID}); err != nil {
		t.Fatalf("the holder's heartbeat: %v", err)
	}
	if _, err := as(builtinToken).Finish(ctx, runner.FinishRequest{Task: ref, RunID: c.RunID, Outcome: v1alpha1.RunOutcomeDone}); err != nil {
		t.Fatalf("the holder's finish: %v", err)
	}
	// A retried finish is answered alreadyFinished for the holder only.
	if fin, err := as(builtinToken).Finish(ctx, runner.FinishRequest{Task: ref, RunID: c.RunID, Outcome: v1alpha1.RunOutcomeDone}); err != nil || !fin.AlreadyFinished {
		t.Fatalf("the holder's retried finish: %+v, %v", fin, err)
	}
	if _, err := as(otherToken).Finish(ctx, runner.FinishRequest{Task: ref, RunID: c.RunID, Outcome: v1alpha1.RunOutcomeDone}); !cadreclient.IsCode(err, httpjson.CodeForbidden) {
		t.Fatalf("another identity replaying the ended run's finish: %v, want forbidden", err)
	}
}
