package controller

import (
	"context"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

// reviews is an API server that answers SelfSubjectAccessReviews from
// allowed, by namespace and verb, and counts them.
type reviews struct {
	allowed map[string]bool // namespace/verb
	asked   int
}

func (r *reviews) client(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			review, ok := obj.(*authorizationv1.SelfSubjectAccessReview)
			if !ok {
				return c.Create(ctx, obj, opts...)
			}
			r.asked++
			a := review.Spec.ResourceAttributes
			review.Status.Allowed = r.allowed[a.Namespace+"/"+a.Verb]
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

// Every job-backed Agent in a namespace shares one set of reviews for a
// resync; a binding added or removed shows once it expires.
func TestJobAccessIsAskedOncePerNamespaceAndResync(t *testing.T) {
	ctx := context.Background()
	r := &reviews{allowed: allowAll("team-a")}
	objs := []client.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "jobs"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: "jobs"}},
	}
	for _, ns := range []string{"team-a", "team-b"} {
		objs = append(objs, &v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "jobs"}, Spec: v1alpha1.RunnerSpec{
			ServiceAccount: &v1alpha1.ServiceAccountRef{Name: "jobs"}, Labels: map[string]string{"pool": "jobs"},
		}})
	}
	c := r.client(t, objs...)
	now := time.Unix(1_800_000_000, 0)
	access := newJobAccess(c, time.Minute)
	access.now = func() time.Time { return now }
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

	// Two Agents, each reconciled twice: three reviews in all.
	for range 2 {
		resolve(agent("team-a", "one"), "Resolved")
		resolve(agent("team-a", "two"), "Resolved")
	}
	if r.asked != 3 {
		t.Fatalf("%d reviews for one namespace in one resync, want 3", r.asked)
	}
	// Another namespace is asked about on its own, and refused at the
	// first verb.
	resolve(agent("team-b", "one"), "Forbidden")
	resolve(agent("team-b", "two"), "Forbidden")
	if r.asked != 4 {
		t.Fatalf("%d reviews after a refused namespace, want 4", r.asked)
	}

	// The binding removed in one namespace and added in the other: seen
	// once the answers expire, not before.
	r.allowed = allowAll("team-b")
	now = now.Add(59 * time.Second)
	resolve(agent("team-a", "one"), "Resolved")
	resolve(agent("team-b", "one"), "Forbidden")
	now = now.Add(time.Second)
	resolve(agent("team-a", "one"), "Forbidden")
	resolve(agent("team-a", "two"), "Forbidden")
	resolve(agent("team-b", "one"), "Resolved")
	resolve(agent("team-b", "two"), "Resolved")
	if r.asked != 4+1+3 {
		t.Fatalf("%d reviews after the answers expired, want %d", r.asked, 4+1+3)
	}
	// A namespace not asked about since its answer expired is forgotten
	// at the next review.
	now = now.Add(time.Minute)
	resolve(agent("team-b", "one"), "Resolved")
	if _, kept := access.answers["team-a"]; kept || len(access.answers) != 1 {
		t.Errorf("namespaces kept: %v", access.answers)
	}
}
