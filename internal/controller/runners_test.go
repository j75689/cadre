package controller_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
	"github.com/j75689/cadre/internal/token"
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
	for _, ns := range []string{"team-a", "team-b", "cadre-system"} {
		create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	}
	create(
		&v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: v1alpha1.TeamName}},
		&v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: v1alpha1.TeamName}},
		&v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "laptop"}, Spec: v1alpha1.RunnerSpec{Labels: map[string]string{"pool": "gpu"}}},
		&v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "ci"}, Spec: v1alpha1.RunnerSpec{
			ServiceAccount: &v1alpha1.ServiceAccountRef{Name: "ci"}, Labels: map[string]string{"pool": "ci-a"}}},
		// team-b names a ServiceAccount "ci" too: its own, not team-a's.
		&v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: "ci"}, Spec: v1alpha1.RunnerSpec{
			ServiceAccount: &v1alpha1.ServiceAccountRef{Name: "ci"}, Labels: map[string]string{"pool": "ci-b"}}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "ci"}},
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
	// minted waits for the laptop's token, different from old, and for
	// the Runner's status to name it.
	minted := func(old string) string {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var s corev1.Secret
			var rn v1alpha1.Runner
			if c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "laptop-token"}, &s) == nil &&
				c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "laptop"}, &rn) == nil &&
				string(s.Data["token"]) != old && rn.Status.TokenHash == token.Hash(string(s.Data["token"])) {
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

	auth := controller.Runners(mgr.GetClient(), runner.Cached(controller.TokenReviews(c)), builtinSA)
	tok := minted("")
	id, err := authenticated(ctx, auth, tok)
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
	if err := refused(ctx, auth, tok); err != nil {
		t.Fatalf("the rotated-out token: %v", err)
	}
	if _, err := authenticated(ctx, auth, next); err != nil {
		t.Fatalf("the new token: %v", err)
	}

	// A ServiceAccount claims where its Runners are, in its own namespace,
	// with their labels: team-b's "ci" Runner names team-b's account.
	var ciID runner.Identity
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ciID, err = auth.Authenticate(ctx, saToken("team-a", "ci")); err == nil && len(ciID.Scope) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ciID.Name != "system:serviceaccount:team-a:ci" || len(ciID.Scope) != 1 || ciID.Scope["team-a"]["pool"] != "ci-a" {
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
	ci := cadreclient.New(srv.URL, &http.Client{Transport: bearer(saToken("team-a", "ci"))})
	if c, err := ci.Claim(ctx, runner.ClaimRequest{Runner: "ci", Labels: map[string]string{"pool": "gpu"}}); err != nil || c != nil {
		t.Fatalf("ci claiming with a label it sends: %+v, %v; want nothing", c, err)
	}

	// A Secret of the token's name the Runner does not own is no token:
	// the current one stops, and the Runner says why.
	if err := c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "laptop-token"}}); err != nil {
		t.Fatal(err)
	}
	create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "laptop-token"}, Data: map[string][]byte{"token": []byte(next)}})
	if err := refused(ctx, auth, next); err != nil {
		t.Fatalf("a token in a Secret the Runner does not own: %v", err)
	}
	waitRunner(t, c, "laptop", func(rn *v1alpha1.Runner) bool {
		cond := meta.FindStatusCondition(rn.Status.Conditions, v1alpha1.ConditionReady)
		return rn.Status.TokenHash == "" && cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == "SecretNotOwned"
	})

	// Switching a Runner to a ServiceAccount deletes its own minted
	// Secret, so switching back mints a fresh token.
	create(&v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "switch"}, Spec: v1alpha1.RunnerSpec{Labels: map[string]string{"pool": "x"}}})
	waitRunner(t, c, "switch", func(rn *v1alpha1.Runner) bool { return rn.Status.TokenHash != "" })
	var rn v1alpha1.Runner
	if err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "switch"}, &rn); err != nil {
		t.Fatal(err)
	}
	rn.Spec.ServiceAccount = &v1alpha1.ServiceAccountRef{Name: "ci"}
	if err := c.Update(ctx, &rn); err != nil {
		t.Fatal(err)
	}
	waitRunner(t, c, "switch", func(rn *v1alpha1.Runner) bool { return rn.Status.TokenHash == "" })
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "switch-token"}, &corev1.Secret{})
		if apierrors.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the minted Secret outlived the switch: %v", err)
		}
	}
}

// refused waits for tok to be refused.
func refused(ctx context.Context, auth runner.Authenticator, tok string) error {
	var err error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if _, err = auth.Authenticate(ctx, tok); errors.Is(err, runner.ErrUnauthenticated) {
			return nil
		}
	}
	return fmt.Errorf("still accepted, last %w", err)
}

// waitRunner waits for the Runner name in team-a to satisfy ok.
func waitRunner(t *testing.T, c client.Client, name string, ok func(*v1alpha1.Runner) bool) {
	t.Helper()
	var rn v1alpha1.Runner
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: name}, &rn) == nil && ok(&rn) {
			return
		}
	}
	t.Fatalf("Runner %s never got there: %+v", name, rn.Status)
}

// authenticated waits for tok to authenticate: the Runner cache may lag
// the status write by a moment.
func authenticated(ctx context.Context, auth runner.Authenticator, tok string) (runner.Identity, error) {
	var id runner.Identity
	var err error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if id, err = auth.Authenticate(ctx, tok); err == nil {
			return id, nil
		}
	}
	return id, err
}
