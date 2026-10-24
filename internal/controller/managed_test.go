package controller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/controller"
)

// makeAvailable marks a Deployment's rollout of its current spec complete,
// as the Deployment controller does once its pods are ready; envtest runs
// none.
func (e *env) makeAvailable(ns, name string) {
	e.t.Helper()
	e.deploymentStatus(ns, name, func(dep *appsv1.Deployment) appsv1.DeploymentStatus {
		return appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "MinimumReplicasAvailable"}}}
	})
}

// deploymentStatus writes the status status gives for a Deployment.
func (e *env) deploymentStatus(ns, name string, status func(*appsv1.Deployment) appsv1.DeploymentStatus) {
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
		dep.Status = status(&dep)
		return e.c.Status().Update(context.Background(), &dep)
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// denyDeletes makes the API server refuse, as Forbidden, deleting the
// Deployment name in ns, until the function it returns is called.
func (e *env) denyDeletes(ns, name string) func() {
	e.t.Helper()
	ctx := context.Background()
	policy := &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "deny-delete-" + ns}, Spec: admissionv1.ValidatingAdmissionPolicySpec{
		MatchConstraints: &admissionv1.MatchResources{ResourceRules: []admissionv1.NamedRuleWithOperations{{
			ResourceNames: []string{name},
			RuleWithOperations: admissionv1.RuleWithOperations{
				Operations: []admissionv1.OperationType{admissionv1.Delete},
				Rule:       admissionv1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1"}, Resources: []string{"deployments"}},
			},
		}}},
		Validations: []admissionv1.Validation{{Expression: "false", Reason: ptr.To(metav1.StatusReasonForbidden), Message: "deletes denied by the test"}},
	}}
	binding := &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: policy.Name}, Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{
		PolicyName:        policy.Name,
		ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny},
		MatchResources: &admissionv1.MatchResources{NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{corev1.LabelMetadataName: ns},
		}},
	}}
	e.create(policy, binding)
	// The policy takes effect a moment after it is created.
	key := client.ObjectKey{Namespace: ns, Name: name}
	eventually(e.t, 10*time.Second, func() (bool, string) {
		var dep appsv1.Deployment
		if err := e.c.Get(ctx, key, &dep); err != nil {
			return false, err.Error()
		}
		err := e.c.Delete(ctx, &dep, client.DryRunAll)
		return apierrors.IsForbidden(err), "a dry-run delete gave " + fmt.Sprint(err)
	})
	return func() {
		e.t.Helper()
		for _, obj := range []client.Object{binding, policy} {
			if err := e.c.Delete(ctx, obj); err != nil {
				e.t.Fatal(err)
			}
		}
	}
}

// stays fails unless obj's Ready condition keeps reason for a while.
func (e *env) stays(obj client.Object, status metav1.ConditionStatus, reason string) {
	e.t.Helper()
	time.Sleep(time.Second) // several resyncs
	e.ready(obj, status, reason)
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
	// The new spec not seen yet: an earlier rollout's failure is not its.
	e.deploymentStatus(ns, "search", func(dep *appsv1.Deployment) appsv1.DeploymentStatus {
		return appsv1.DeploymentStatus{ObservedGeneration: dep.Generation - 1, Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1, Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"},
		}}
	})
	e.stays(ts, metav1.ConditionFalse, "Deploying")
	// Mid-rollout: the new spec seen, the old pod still serving and
	// Available. Nothing runs the new image yet.
	e.deploymentStatus(ns, "search", func(dep *appsv1.Deployment) appsv1.DeploymentStatus {
		return appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 0,
			Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "MinimumReplicasAvailable"}}}
	})
	e.stays(ts, metav1.ConditionFalse, "Deploying")
	e.makeAvailable(ns, "search")
	e.ready(ts, metav1.ConditionTrue, "Available")

	// A rollout that cannot finish says so.
	e.deploymentStatus(ns, "search", func(dep *appsv1.Deployment) appsv1.DeploymentStatus {
		return appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1, Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "MinimumReplicasAvailable"},
			{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded", Message: "ReplicaSet has timed out progressing"},
		}}
	})
	e.ready(ts, metav1.ConditionFalse, "RolloutFailed")
	e.makeAvailable(ns, "search")
	e.ready(ts, metav1.ConditionTrue, "Available")

	// Made external where cadre-server may not delete what it ran, it is
	// not Ready and keeps the URL until it may.
	undeny := e.denyDeletes(ns, "search")
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := e.c.Get(ctx, client.ObjectKeyFromObject(ts), ts); err != nil {
			return err
		}
		ts.Spec.Managed, ts.Spec.URL = nil, "https://search.example.test/mcp"
		return e.c.Update(ctx, ts)
	}); err != nil {
		t.Fatal(err)
	}
	e.ready(ts, metav1.ConditionFalse, "Forbidden")
	if ts.Status.URL == "" {
		t.Error("status.url cleared while the Deployment could not be deleted")
	}
	// Once it may, it loses what Cadre ran for it.
	undeny()
	e.ready(ts, metav1.ConditionTrue, "External")
	eventually(t, 10*time.Second, func() (bool, string) {
		depErr := e.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "search"}, &appsv1.Deployment{})
		svcErr := e.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "search"}, &corev1.Service{})
		return apierrors.IsNotFound(depErr) && apierrors.IsNotFound(svcErr), "the Deployment or the Service is still there"
	})
	if ts.Status.URL != "" {
		t.Errorf("status.url %q once external", ts.Status.URL)
	}
}

// A Deployment or Service of a managed ToolServer's name that is not its
// own, labelled like Cadre's or not, is left alone, and the ToolServer is not
// Ready.
func TestAManagedToolServersNameTaken(t *testing.T) {
	e := start(t)
	const ns = "team-taken"
	labels := func(name string) map[string]string { return map[string]string{controller.LabelToolServer: name} }
	replicas := int32(1)
	unlabelled := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "unlabelled"}, Spec: appsv1.DeploymentSpec{
		Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "other"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "other", Image: "example.test/other:1"}}}},
	}}
	foreign := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "labelled", Labels: labels("labelled")}, Spec: appsv1.DeploymentSpec{
		Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels("labelled")},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels("labelled")}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "other", Image: "example.test/other:1"}}}},
	}}
	service := &corev1.Service{ObjectMeta: meta2(ns, "service"), Spec: corev1.ServiceSpec{
		Selector: map[string]string{"app": "other"}, Ports: []corev1.ServicePort{{Port: 9000}},
	}}
	e.create(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&v1alpha1.Team{ObjectMeta: meta2(ns, v1alpha1.TeamName)},
		unlabelled, foreign, service,
	)
	for _, name := range []string{"unlabelled", "labelled", "service"} {
		ts := &v1alpha1.ToolServer{ObjectMeta: meta2(ns, name), Spec: v1alpha1.ToolServerSpec{
			Managed: &v1alpha1.ManagedServer{Image: "example.test/search:1.0", Port: 8080},
		}}
		e.create(ts)
		e.ready(ts, metav1.ConditionFalse, "NameTaken")
	}
	ctx := context.Background()
	for _, d := range []*appsv1.Deployment{unlabelled, foreign} {
		var dep appsv1.Deployment
		if err := e.c.Get(ctx, client.ObjectKeyFromObject(d), &dep); err != nil || dep.Spec.Template.Spec.Containers[0].Image != "example.test/other:1" ||
			len(dep.OwnerReferences) != 0 || !equality.Semantic.DeepEqual(dep.Labels, d.Labels) {
			t.Errorf("the foreign Deployment %s was touched: %+v, labels %v, owners %v, %v", d.Name, dep.Spec.Template.Spec.Containers, dep.Labels, dep.OwnerReferences, err)
		}
	}
	var svc corev1.Service
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(service), &svc); err != nil || svc.Spec.Ports[0].Port != 9000 || svc.Spec.Selector["app"] != "other" || len(svc.OwnerReferences) != 0 {
		t.Errorf("the foreign Service was touched: %+v, %v", svc.Spec, err)
	}
}
