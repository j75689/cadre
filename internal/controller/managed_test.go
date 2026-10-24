package controller_test

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/controller"
)

// makeAvailable marks a Deployment Available for its current spec, as the
// Deployment controller does once its pods are ready; envtest runs none.
func (e *env) makeAvailable(ns, name string) {
	e.t.Helper()
	key := client.ObjectKey{Namespace: ns, Name: name}
	var dep appsv1.Deployment
	eventually(e.t, 10*time.Second, func() (bool, string) {
		return e.c.Get(context.Background(), key, &dep) == nil, "no Deployment " + name
	})
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := e.c.Get(context.Background(), key, &dep); err != nil {
			return err
		}
		dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "MinimumReplicasAvailable"}}}
		return e.c.Status().Update(context.Background(), &dep)
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// A managed ToolServer runs as a Deployment and a Service named after it
// and owned by it, is reached at its Service, and is Ready while its
// Deployment is Available for its current spec.
func TestAManagedToolServer(t *testing.T) {
	e := start(t)
	const ns = "team-managed"
	e.create(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&v1alpha1.Team{ObjectMeta: meta2(ns, v1alpha1.TeamName)},
	)
	ts := &v1alpha1.ToolServer{ObjectMeta: meta2(ns, "search"), Spec: v1alpha1.ToolServerSpec{
		Managed: &v1alpha1.ManagedServer{Image: "example.test/search:1.0", Port: 8080},
	}}
	e.create(ts)
	e.ready(ts, metav1.ConditionFalse, "Deploying")
	if ts.Status.URL != "http://search.team-managed.svc:8080/mcp" {
		t.Fatalf("status.url %q", ts.Status.URL)
	}

	var dep appsv1.Deployment
	var svc corev1.Service
	ctx := context.Background()
	if err := e.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "search"}, &dep); err != nil {
		t.Fatal(err)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	switch {
	case *dep.Spec.Replicas != 1 || c.Image != "example.test/search:1.0" || c.Ports[0].ContainerPort != 8080:
		t.Errorf("deployment %+v", dep.Spec)
	case dep.Labels[controller.LabelToolServer] != "search" || dep.Spec.Template.Labels[controller.LabelToolServer] != "search":
		t.Errorf("labels %v / %v", dep.Labels, dep.Spec.Template.Labels)
	case !metav1.IsControlledBy(&dep, ts):
		// Deleting the ToolServer deletes it through this ownership (the
		// garbage collector, which envtest does not run).
		t.Errorf("owners %+v", dep.OwnerReferences)
	}
	eventually(t, 10*time.Second, func() (bool, string) {
		return e.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "search"}, &svc) == nil, "no Service"
	})
	p := svc.Spec.Ports[0]
	if svc.Spec.Type != corev1.ServiceTypeClusterIP || p.Port != 8080 || p.TargetPort.IntValue() != 8080 || svc.Spec.Selector[controller.LabelToolServer] != "search" || !metav1.IsControlledBy(&svc, ts) {
		t.Errorf("service %+v, owners %+v", svc.Spec, svc.OwnerReferences)
	}

	e.makeAvailable(ns, "search")
	e.ready(ts, metav1.ConditionTrue, "Available")

	// A new image: the Deployment follows, and until it is Available again
	// the server is not Ready.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := e.c.Get(ctx, client.ObjectKeyFromObject(ts), ts); err != nil {
			return err
		}
		ts.Spec.Managed.Image, ts.Spec.Managed.Path = "example.test/search:2.0", "/rpc"
		return e.c.Update(ctx, ts)
	}); err != nil {
		t.Fatal(err)
	}
	e.ready(ts, metav1.ConditionFalse, "Deploying")
	eventually(t, 10*time.Second, func() (bool, string) {
		if err := e.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "search"}, &dep); err != nil {
			return false, err.Error()
		}
		return dep.Spec.Template.Spec.Containers[0].Image == "example.test/search:2.0", "image " + dep.Spec.Template.Spec.Containers[0].Image
	})
	if ts.Status.URL != "http://search.team-managed.svc:8080/rpc" {
		t.Errorf("status.url %q after the path changed", ts.Status.URL)
	}
	e.makeAvailable(ns, "search")
	e.ready(ts, metav1.ConditionTrue, "Available")
}
