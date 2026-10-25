package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

// reviews is an API server that answers SelfSubjectAccessReviews on Jobs
// from allowed, by namespace and verb, and counts them. fail, when set,
// fails the next one; evalError answers it with that evaluation error too,
// and denied makes it an explicit denial.
type reviews struct {
	t         *testing.T
	allowed   map[string]bool // namespace/verb
	asked     int
	fail      error
	evalError string
	denied    bool
}

func (r *reviews) client(objs ...client.Object) client.Client {
	r.t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		r.t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		r.t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&v1alpha1.Agent{}).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			review, ok := obj.(*authorizationv1.SelfSubjectAccessReview)
			if !ok {
				return c.Create(ctx, obj, opts...)
			}
			r.asked++
			a := review.Spec.ResourceAttributes
			if a.Group != "batch" || a.Resource != "jobs" {
				r.t.Errorf("a review of %s/%s, not batch/jobs", a.Group, a.Resource)
			}
			if err := r.fail; err != nil {
				r.fail = nil
				return err
			}
			review.Status.Allowed = r.allowed[a.Namespace+"/"+a.Verb]
			review.Status.EvaluationError, review.Status.Denied = r.evalError, r.denied
			return nil
		},
	}).Build()
}

func allowAll(ns string) map[string]bool {
	m := map[string]bool{}
	for _, v := range jobVerbs {
		m[ns+"/"+v] = true
	}
	return m
}

// Every job-backed Agent in a namespace shares one allowance for a resync,
// and checks again when it expires; a refusal, or no answer, is asked
// again at once.
func TestJobAccess(t *testing.T) {
	ctx := context.Background()
	r := &reviews{t: t, allowed: allowAll("team-a")}
	var objs []client.Object
	for _, ns := range []string{"team-a", "team-b"} {
		objs = append(objs, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "jobs"}},
			&v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "jobs"}, Spec: v1alpha1.RunnerSpec{
				ServiceAccount: &v1alpha1.ServiceAccountRef{Name: "jobs"}, Labels: map[string]string{"pool": "jobs"},
			}})
	}
	c := r.client(objs...)
	now := time.Unix(1_800_000_000, 0)
	access := newJobAccess(c, time.Minute, func() time.Time { return now })
	rec := &agentReconciler{base: base{client: c, secrets: c, resync: time.Minute}, jobs: access}
	agent := func(ns, name string) *v1alpha1.Agent {
		return &v1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: v1alpha1.AgentSpec{
			Runtime: v1alpha1.AgentRuntime{Job: &v1alpha1.AgentJob{RunnerRef: "jobs"}},
		}}
	}
	resolve := func(a *v1alpha1.Agent, want string) {
		t.Helper()
		ok, reason, msg, err := rec.resolveJob(ctx, a, a.Spec.Runtime.Job)
		if err != nil {
			t.Fatal(err)
		}
		if reason != want || ok != (want == "Resolved") {
			t.Fatalf("%s/%s: %v %s %q, want %s", a.Namespace, a.Name, ok, reason, msg, want)
		}
	}
	asked := func(want int, when string) {
		t.Helper()
		if r.asked != want {
			t.Fatalf("%d reviews %s, want %d", r.asked, when, want)
		}
	}

	// Two Agents, each reconciled twice: three reviews in all, and both
	// check again when the allowance expires.
	for range 2 {
		resolve(agent("team-a", "one"), "Resolved")
		resolve(agent("team-a", "two"), "Resolved")
	}
	asked(3, "for one namespace in one resync")
	if d := rec.recheck(agent("team-a", "one")); d != time.Minute {
		t.Errorf("recheck in %v at the start of the allowance", d)
	}
	if d := rec.recheck(&v1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "builtin"}}); d != time.Minute {
		t.Errorf("an Agent without Jobs checks again in %v, not at resync", d)
	}
	now = now.Add(59 * time.Second)
	if d := rec.recheck(agent("team-a", "two")); d != time.Second {
		t.Errorf("an Agent resolved late in the allowance checks again in %v, not when it expires", d)
	}
	// At the very end of it, soon, never not at all.
	access.allowed["team-a"] = now.Add(time.Nanosecond)
	if d := rec.recheck(agent("team-a", "two")); d != 100*time.Millisecond {
		t.Errorf("an Agent resolved as the allowance expires checks again in %v", d)
	}
	access.allowed["team-a"] = now.Add(time.Second)

	// A refusal is not kept: a binding added shows on the next resolve,
	// with no time passing.
	resolve(agent("team-b", "one"), "Forbidden")
	resolve(agent("team-b", "two"), "Forbidden")
	asked(5, "for a refused namespace")
	if d := rec.recheck(agent("team-b", "one")); d != time.Minute {
		t.Errorf("a refused Agent checks again in %v", d)
	}
	r.allowed = allowAll("team-b")
	resolve(agent("team-b", "one"), "Resolved")
	asked(8, "once the namespace is allowed")

	// A binding removed: seen once the allowance expires, not before.
	r.allowed = allowAll("team-b")
	resolve(agent("team-a", "one"), "Resolved")
	now = now.Add(time.Second)
	resolve(agent("team-a", "one"), "Forbidden")
	resolve(agent("team-a", "two"), "Forbidden")
	asked(10, "after the allowance expired")

	// An authorizer that cannot decide, or a review that fails, is an
	// error, AccessUnknown, and nothing is kept: the next reconcile asks
	// again.
	now = now.Add(time.Minute) // team-b's allowance expired too
	for _, fail := range []func(){
		func() { r.allowed, r.evalError = map[string]bool{}, "webhook authorizer timed out" },
		func() { r.fail = errors.New("connection refused") },
	} {
		fail()
		ok, reason, _, err := rec.resolveJob(ctx, agent("team-b", "one"), agent("team-b", "one").Spec.Runtime.Job)
		if err == nil || ok || reason != "AccessUnknown" {
			t.Fatalf("%v %s %v when the review could not be made", ok, reason, err)
		}
		if _, kept := access.expiry("team-b"); kept {
			t.Fatal("an answer kept from a review that failed")
		}
	}
	// An explicit denial with an error is a refusal; an allowance with an
	// error from another authorizer is an allowance.
	r.denied = true
	resolve(agent("team-b", "one"), "Forbidden")
	r.denied, r.allowed = false, allowAll("team-b")
	before := r.asked
	resolve(agent("team-b", "one"), "Resolved")
	asked(before+3, "after the failed reviews")
	r.evalError = ""

	// A namespace not asked about since its allowance expired is
	// forgotten at the next review.
	now = now.Add(time.Minute)
	resolve(agent("team-b", "one"), "Resolved")
	if len(access.allowed) != 1 {
		t.Errorf("namespaces kept: %v", access.allowed)
	}
}

// The RBAC hint follows a refusal by RBAC only.
func TestForbiddenMessage(t *testing.T) {
	gr := schema.GroupResource{Group: "apps", Resource: "deployments"}
	for _, c := range []struct {
		err  error
		hint bool
	}{
		{apierrors.NewForbidden(gr, "search", errors.New(`User "system:serviceaccount:cadre-system:cadre-server" cannot create resource "deployments" in API group "apps" in the namespace "team-a"`)), true},
		{apierrors.NewForbidden(gr, "search", errors.New(`exceeded quota: compute, requested: pods=1, used: pods=10, limited: pods=10`)), false},
		{apierrors.NewForbidden(gr, "search", errors.New(`admission webhook "policy.example.test" denied the request: no latest tags`)), false},
	} {
		got := forbiddenMessage(c.err, "bind the ClusterRole")
		if want := c.err.Error(); c.hint {
			want += "; bind the ClusterRole"
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		} else if got != want {
			t.Errorf("got %q, want the API server's message alone", got)
		}
	}
}

// An Agent whose Job access cannot be checked is not Ready, says why, and
// is Ready again once it can be.
func TestJobAccessUnknown(t *testing.T) {
	ctx := context.Background()
	r := &reviews{t: t, allowed: allowAll("team-a")}
	ready := v1alpha1.ConfigStatus{Conditions: []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "WorkloadIdentity"}}}
	a := &v1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "coder"}, Spec: v1alpha1.AgentSpec{
		ModelRef: "claude", Runtime: v1alpha1.AgentRuntime{Job: &v1alpha1.AgentJob{RunnerRef: "jobs"}},
	}}
	c := r.client(
		&v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: v1alpha1.TeamName}},
		&v1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "claude"}, Status: ready},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "jobs"}},
		&v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "jobs"}, Spec: v1alpha1.RunnerSpec{
			ServiceAccount: &v1alpha1.ServiceAccountRef{Name: "jobs"}, Labels: map[string]string{"pool": "jobs"},
		}},
		a,
	)
	rec := &agentReconciler{base: base{client: c, secrets: c, resync: time.Minute}, jobs: newJobAccess(c, time.Minute, time.Now)}
	reason := func(wantErr bool) string {
		t.Helper()
		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(a)})
		if (err != nil) != wantErr {
			t.Fatalf("reconcile: %v", err)
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(a), a); err != nil {
			t.Fatal(err)
		}
		cond := meta.FindStatusCondition(a.Status.Conditions, v1alpha1.ConditionReady)
		if cond == nil {
			t.Fatal("no Ready condition")
		}
		return cond.Reason + ": " + cond.Message
	}
	if got := reason(false); got != "Resolved: " {
		t.Fatalf("Ready %q", got)
	}
	// Its allowance expired and the authorizer down: not Ready, and the
	// error returned for the backoff.
	rec.jobs = newJobAccess(c, time.Minute, time.Now)
	r.fail = errors.New("connection refused")
	if got := reason(true); !strings.HasPrefix(got, "AccessUnknown: ") || !strings.Contains(got, "connection refused") {
		t.Fatalf("Ready %q while the review fails", got)
	}
	if got := reason(false); got != "Resolved: " {
		t.Fatalf("Ready %q once the review works", got)
	}
}
