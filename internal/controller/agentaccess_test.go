package controller_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/testenv"
	"github.com/j75689/cadre/internal/token"
)

// AgentAccesses are reconciled as Runners are: a minted token in a Secret
// the AgentAccess owns, rotated by deleting it; one AgentAccess per
// ServiceAccount; and nothing outside a Team namespace.
func TestAgentAccesses(t *testing.T) {
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
	create := func(objs ...client.Object) {
		t.Helper()
		for _, o := range objs {
			if err := c.Create(ctx, o); err != nil {
				t.Fatalf("create %T %s: %v", o, o.GetName(), err)
			}
		}
	}
	access := func(ns, name, sa string) *v1alpha1.AgentAccess {
		aa := &v1alpha1.AgentAccess{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: v1alpha1.AgentAccessSpec{Role: "member"}}
		if sa != "" {
			aa.Spec.ServiceAccount = &v1alpha1.ServiceAccountRef{Name: sa}
		}
		return aa
	}
	for _, ns := range []string{"team-a", "no-team"} {
		create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	}
	create(&v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: v1alpha1.TeamName}}, access("team-a", "bot", ""))

	// minted waits for bot's token, different from old, and for its
	// status to name it.
	minted := func(old string) string {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			var s corev1.Secret
			var aa v1alpha1.AgentAccess
			if c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "bot-access-token"}, &s) == nil &&
				c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "bot"}, &aa) == nil &&
				string(s.Data["token"]) != old && aa.Status.TokenHash == token.Hash(string(s.Data["token"])) {
				ns, name, ok := token.Locate(string(s.Data["token"]))
				if s.Labels[v1alpha1.LabelAgentAccess] != "bot" || len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != aa.UID ||
					aa.Status.TokenSecret != "bot-access-token" || aa.Status.MintedAt == nil || !ok || ns != "team-a" || name != "bot" {
					t.Fatalf("secret %+v, status %+v", s.ObjectMeta, aa.Status)
				}
				return string(s.Data["token"])
			}
		}
		t.Fatal("no token was minted")
		return ""
	}
	tok := minted("")
	waitAccess(t, c, "team-a", "bot", ready(true, "TokenMinted"))

	// Deleting the Secret rotates the token.
	if err := c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot-access-token"}}); err != nil {
		t.Fatal(err)
	}
	minted(tok)

	// A Runner and an AgentAccess of the same name have a Secret each:
	// rotating one's token leaves the other's alone.
	create(&v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "shared"}, Spec: v1alpha1.RunnerSpec{Labels: map[string]string{"pool": "x"}}},
		access("team-a", "shared", ""))
	waitRunner(t, c, "shared", func(rn *v1alpha1.Runner) bool {
		return rn.Status.TokenSecret == "shared-token" && rn.Status.TokenHash != ""
	})
	waitAccess(t, c, "team-a", "shared", func(aa *v1alpha1.AgentAccess) bool {
		return ready(true, "TokenMinted")(aa) && aa.Status.TokenSecret == "shared-access-token"
	})
	hashes := func() (runner, access string) {
		t.Helper()
		var rn v1alpha1.Runner
		var aa v1alpha1.AgentAccess
		if err := errors.Join(c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "shared"}, &rn), c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "shared"}, &aa)); err != nil {
			t.Fatal(err)
		}
		return rn.Status.TokenHash, aa.Status.TokenHash
	}
	runnerHash, accessHash := hashes()
	for _, rotate := range []struct{ secret, kind string }{{"shared-token", "Runner"}, {"shared-access-token", "AgentAccess"}} {
		if err := c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: rotate.secret}}); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			r, a := hashes()
			if rotate.kind == "Runner" && r != runnerHash && r != "" && a == accessHash {
				runnerHash = r
				break
			}
			if rotate.kind == "AgentAccess" && a != accessHash && a != "" && r == runnerHash {
				accessHash = a
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("deleting %s: hashes %s, %s from %s, %s", rotate.secret, r, a, runnerHash, accessHash)
			}
		}
	}
	// Settled for a few resyncs, neither token moved again.
	time.Sleep(time.Second)
	if r, a := hashes(); r != runnerHash || a != accessHash {
		t.Fatalf("hashes moved: %s, %s from %s, %s", r, a, runnerHash, accessHash)
	}
	waitAccess(t, c, "team-a", "shared", ready(true, "TokenMinted"))
	waitRunner(t, c, "shared", func(rn *v1alpha1.Runner) bool {
		cond := meta.FindStatusCondition(rn.Status.Conditions, v1alpha1.ConditionReady)
		return cond != nil && cond.Status == metav1.ConditionTrue && cond.Reason == "TokenMinted"
	})

	// Where the two Secret names coincide, Runner p-access's and
	// AgentAccess p's, the Runner holds it, whichever came first, and the
	// Secret re-minted after a rotation is the Runner's.
	for _, runnerFirst := range []bool{true, false} {
		name := "pa"
		if !runnerFirst {
			name = "pb"
		}
		secret, runnerName := name+"-access-token", name+"-access"
		newRunner := &v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: runnerName}, Spec: v1alpha1.RunnerSpec{Labels: map[string]string{"pool": "x"}}}
		if runnerFirst {
			create(newRunner)
			waitRunner(t, c, runnerName, func(rn *v1alpha1.Runner) bool { return rn.Status.TokenHash != "" })
			create(access("team-a", name, ""))
		} else {
			create(access("team-a", name, ""))
			waitAccess(t, c, "team-a", name, ready(true, "TokenMinted"))
			create(newRunner)
		}
		taken := func(aa *v1alpha1.AgentAccess) bool {
			return ready(false, "SecretNameTaken")(aa) && aa.Status.TokenHash == "" && aa.Status.TokenSecret == "" &&
				strings.Contains(meta.FindStatusCondition(aa.Status.Conditions, v1alpha1.ConditionReady).Message, "is Runner "+runnerName+"'s token")
		}
		waitAccess(t, c, "team-a", name, taken)
		var rn v1alpha1.Runner
		held := func(old string) {
			t.Helper()
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
				var s corev1.Secret
				if c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: runnerName}, &rn) == nil &&
					c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: secret}, &s) == nil &&
					len(s.OwnerReferences) == 1 && s.OwnerReferences[0].UID == rn.UID && s.OwnerReferences[0].Kind == "Runner" &&
					rn.Status.TokenHash == token.Hash(string(s.Data["token"])) && rn.Status.TokenHash != old {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("Runner %s never held %s: %+v", runnerName, secret, rn.Status)
				}
			}
		}
		held("")
		first := rn.Status.TokenHash
		if err := c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: secret}}); err != nil {
			t.Fatal(err)
		}
		held(first)
		waitAccess(t, c, "team-a", name, taken)
	}

	// One ServiceAccount is one AgentAccess's: the oldest, then by name.
	create(access("team-a", "first", "agent-sa"), access("team-a", "second", "agent-sa"))
	waitAccess(t, c, "team-a", "first", func(aa *v1alpha1.AgentAccess) bool {
		return ready(true, "ServiceAccount")(aa) && aa.Status.TokenSecret == "" && aa.Status.TokenHash == ""
	})
	waitAccess(t, c, "team-a", "second", func(aa *v1alpha1.AgentAccess) bool {
		return ready(false, "ServiceAccountInUse")(aa) &&
			strings.Contains(meta.FindStatusCondition(aa.Status.Conditions, v1alpha1.ConditionReady).Message, "AgentAccess first's")
	})
	if err := c.Delete(ctx, access("team-a", "first", "")); err != nil {
		t.Fatal(err)
	}
	waitAccess(t, c, "team-a", "second", ready(true, "ServiceAccount"))

	// Switching to a ServiceAccount deletes the minted Secret, so switching
	// back mints a fresh token.
	var bot v1alpha1.AgentAccess
	if err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "bot"}, &bot); err != nil {
		t.Fatal(err)
	}
	bot.Spec.ServiceAccount = &v1alpha1.ServiceAccountRef{Name: "bot-sa"}
	if err := c.Update(ctx, &bot); err != nil {
		t.Fatal(err)
	}
	waitAccess(t, c, "team-a", "bot", func(aa *v1alpha1.AgentAccess) bool {
		return ready(true, "ServiceAccount")(aa) && aa.Status.TokenHash == ""
	})
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "bot-access-token"}, &corev1.Secret{})
		if apierrors.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the minted Secret outlived the switch: %v", err)
		}
	}

	// Outside a Team namespace nothing is minted.
	create(access("no-team", "bot", ""))
	waitAccess(t, c, "no-team", "bot", ready(false, "NoTeam"))
	if err := c.Get(ctx, client.ObjectKey{Namespace: "no-team", Name: "bot-access-token"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a token Secret outside a Team namespace: %v", err)
	}
}

// ready is whether an AgentAccess's Ready condition, for its current spec,
// has status and reason.
func ready(status bool, reason string) func(*v1alpha1.AgentAccess) bool {
	return func(aa *v1alpha1.AgentAccess) bool {
		cond := meta.FindStatusCondition(aa.Status.Conditions, v1alpha1.ConditionReady)
		want := metav1.ConditionFalse
		if status {
			want = metav1.ConditionTrue
		}
		return cond != nil && cond.Status == want && cond.Reason == reason && cond.ObservedGeneration == aa.Generation
	}
}

// waitAccess waits for the AgentAccess ns/name to satisfy ok.
func waitAccess(t *testing.T, c client.Client, ns, name string, ok func(*v1alpha1.AgentAccess) bool) {
	t.Helper()
	var aa v1alpha1.AgentAccess
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &aa) == nil && ok(&aa) {
			return
		}
	}
	t.Fatalf("AgentAccess %s/%s never got there: %+v", ns, name, aa.Status)
}
