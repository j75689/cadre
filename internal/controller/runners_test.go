package controller_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	cadreclient "github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/storage/memory"
	"github.com/j75689/cadre/internal/testenv"
)

const builtinSA = "system:serviceaccount:cadre-system:cadre-runner"

// Runners: a runner outside the cluster gets a minted token, rotated by
// deleting its Secret; a ServiceAccount's Runners say where it may claim
// and with which labels; and a claim follows the Runner, not the request.
func TestRunners(t *testing.T) {
	cfg := testenv.APIServer(t)
	mgr, err := controller.NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Setup(mgr, controller.Options{Resync: 200 * time.Millisecond}, controller.SkipNameValidation()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
	c, err := client.New(cfg, client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		t.Fatal(err)
	}
	cs := kubernetes.NewForConfigOrDie(cfg)
	create := func(objs ...client.Object) {
		t.Helper()
		for _, o := range objs {
			if err := c.Create(ctx, o); err != nil {
				t.Fatalf("create %T %s: %v", o, o.GetName(), err)
			}
		}
	}
	for _, ns := range []string{"team-a", "team-b", "bots", "cadre-system"} {
		create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	}
	create(
		&v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: v1alpha1.TeamName}},
		&v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: v1alpha1.TeamName}},
		&v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "laptop"}, Spec: v1alpha1.RunnerSpec{Labels: map[string]string{"pool": "gpu"}}},
		&v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "ci"}, Spec: v1alpha1.RunnerSpec{
			ServiceAccount: &v1alpha1.ServiceAccountRef{Namespace: "bots", Name: "ci"}, Labels: map[string]string{"pool": "ci-a"}}},
		&v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: "ci"}, Spec: v1alpha1.RunnerSpec{
			ServiceAccount: &v1alpha1.ServiceAccountRef{Namespace: "bots", Name: "ci"}, Labels: map[string]string{"pool": "ci-b"}}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "bots", Name: "ci"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "cadre-system", Name: "cadre-runner"}},
	)
	saToken := func(ns, name string) string {
		t.Helper()
		tr, err := cs.CoreV1().ServiceAccounts(ns).CreateToken(ctx, name, &authnv1.TokenRequest{Spec: authnv1.TokenRequestSpec{Audiences: []string{controller.RunnerAudience}}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return tr.Status.Token
	}
	// minted waits for the laptop's token, different from old.
	minted := func(old string) string {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var s corev1.Secret
			var rn v1alpha1.Runner
			if c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "laptop-token"}, &s) == nil &&
				c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "laptop"}, &rn) == nil &&
				rn.Status.TokenHash != "" && string(s.Data["token"]) != old {
				if s.Labels[v1alpha1.LabelRunner] != "laptop" || len(s.OwnerReferences) != 1 || rn.Status.TokenSecret != "laptop-token" {
					t.Fatalf("secret %+v, status %+v", s.ObjectMeta, rn.Status)
				}
				return string(s.Data["token"])
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("no token was minted")
		return ""
	}

	auth := controller.Runners(mgr.GetClient(), mgr.GetAPIReader(), runner.Cached(controller.TokenReviews(c)), builtinSA)
	tok := minted("")
	id, err := auth.Authenticate(ctx, tok)
	if err != nil || id.Name != "cadre:runner:team-a/laptop" || len(id.Scope) != 1 || id.Scope["team-a"]["pool"] != "gpu" {
		t.Fatalf("minted token: %+v, %v", id, err)
	}
	for _, bad := range []string{tok + "x", "dGVhbS1hL2xhcHRvcA.AAAA", "dGVhbS1hL25vYm9keQ." + tok[len(tok)-43:]} {
		if _, err := auth.Authenticate(ctx, bad); !errors.Is(err, runner.ErrUnauthenticated) {
			t.Errorf("token %q: %v, want unauthenticated", bad, err)
		}
	}

	// Deleting the Secret rotates the token: the old one stops at once.
	if err := c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "laptop-token"}}); err != nil {
		t.Fatal(err)
	}
	next := minted(tok)
	if _, err := auth.Authenticate(ctx, tok); !errors.Is(err, runner.ErrUnauthenticated) {
		t.Fatalf("the rotated-out token: %v, want unauthenticated", err)
	}
	if _, err := auth.Authenticate(ctx, next); err != nil {
		t.Fatalf("the new token: %v", err)
	}

	// A ServiceAccount claims where its Runners are, with their labels.
	var ciID runner.Identity
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ciID, err = auth.Authenticate(ctx, saToken("bots", "ci")); err == nil && len(ciID.Scope) == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ciID.Name != "system:serviceaccount:bots:ci" || ciID.Scope["team-a"]["pool"] != "ci-a" || ciID.Scope["team-b"]["pool"] != "ci-b" {
		t.Fatalf("ci's identity: %+v, %v", ciID, err)
	}
	if b, err := auth.Authenticate(ctx, saToken("cadre-system", "cadre-runner")); err != nil || b.Scope != nil {
		t.Fatalf("cadre-runner: %+v, %v; want everywhere", b, err)
	}

	// The claim follows the laptop's Runner: its labels, its namespace,
	// whatever it sends.
	agents := manager.StaticAgents{Agents: map[string]*v1alpha1.Agent{}, ModelProviders: map[string]*v1alpha1.ModelProvider{}}
	for _, ns := range []string{"team-a", "team-b"} {
		agents.Agents[ns+"/gpu-worker"] = manager.ReadyAgent(ns, "gpu-worker", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeRunner, Selector: map[string]string{"pool": "gpu"}})
		mp := &v1alpha1.ModelProvider{Spec: v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderAnthropic, Model: "claude-opus-5-5"}}
		mp.Namespace, mp.Name = ns, "claude"
		agents.ModelProviders[ns+"/claude"] = mp
	}
	m := manager.New(memory.NewBackend().Open(), manager.Config{Agents: agents})
	mux := http.NewServeMux()
	runner.NewServer(m, slog.New(slog.NewTextHandler(io.Discard, nil)), "http://cadre.test", runner.WithAuth(auth, builtinSA)).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	for _, ns := range []string{"team-a", "team-b"} {
		if _, err := m.InitTeam(ctx, ns, v1alpha1.TeamSpec{MaxConcurrentRuns: 5}); err != nil {
			t.Fatal(err)
		}
		if _, err := m.CreateTask(ctx, ns, "render", v1alpha1.TaskSpec{Title: "render", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "gpu-worker"}}); err != nil {
			t.Fatal(err)
		}
	}
	laptop := cadreclient.New(srv.URL, &http.Client{Transport: bearer(next)})
	// It sends no labels, and asks for team-b too: only team-a's task is
	// its to claim, with the Runner's pool label.
	claim := runner.ClaimRequest{Runner: "my-laptop", Namespaces: []string{"team-a", "team-b"}}
	got, err := laptop.Claim(ctx, claim)
	if err != nil || got == nil || got.Task.Namespace != "team-a" {
		t.Fatalf("the laptop's claim: %+v, %v", got, err)
	}
	if again, err := laptop.Claim(ctx, claim); err != nil || again != nil {
		t.Fatalf("a second claim: %+v, %v; team-b is not the laptop's", again, err)
	}
	// ci's labels (ci-a, ci-b) match no gpu agent, whatever it sends.
	ci := cadreclient.New(srv.URL, &http.Client{Transport: bearer(saToken("bots", "ci"))})
	if c, err := ci.Claim(ctx, runner.ClaimRequest{Runner: "ci", Labels: map[string]string{"pool": "gpu"}}); err != nil || c != nil {
		t.Fatalf("ci claiming with a label it sends: %+v, %v; want nothing", c, err)
	}
}
