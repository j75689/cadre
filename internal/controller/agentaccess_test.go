package controller_test

import (
	"context"
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
			if c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "bot-token"}, &s) == nil &&
				c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "bot"}, &aa) == nil &&
				string(s.Data["token"]) != old && aa.Status.TokenHash == token.Hash(string(s.Data["token"])) {
				ns, name, ok := token.Locate(string(s.Data["token"]))
				if s.Labels[v1alpha1.LabelAgentAccess] != "bot" || len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != aa.UID ||
					aa.Status.TokenSecret != "bot-token" || aa.Status.MintedAt == nil || !ok || ns != "team-a" || name != "bot" {
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
	if err := c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot-token"}}); err != nil {
		t.Fatal(err)
	}
	minted(tok)

	// A Runner and an AgentAccess of the same name would share the Secret:
	// the second is not Ready, and the first keeps its token.
	create(&v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "shared"}, Spec: v1alpha1.RunnerSpec{Labels: map[string]string{"pool": "x"}}})
	waitRunner(t, c, "shared", func(rn *v1alpha1.Runner) bool { return rn.Status.TokenHash != "" })
	var rn v1alpha1.Runner
	if err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "shared"}, &rn); err != nil {
		t.Fatal(err)
	}
	create(access("team-a", "shared", ""))
	waitAccess(t, c, "team-a", "shared", func(aa *v1alpha1.AgentAccess) bool {
		return ready(false, "SecretNotOwned")(aa) && aa.Status.TokenHash == "" &&
			strings.Contains(meta.FindStatusCondition(aa.Status.Conditions, v1alpha1.ConditionReady).Message, "is Runner shared's token")
	})
	var after v1alpha1.Runner
	if err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "shared"}, &after); err != nil || after.Status.TokenHash != rn.Status.TokenHash {
		t.Fatalf("the Runner's token hash went from %q to %q: %v", rn.Status.TokenHash, after.Status.TokenHash, err)
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
		err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "bot-token"}, &corev1.Secret{})
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
	if err := c.Get(ctx, client.ObjectKey{Namespace: "no-team", Name: "bot-token"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
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
