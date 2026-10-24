package controller

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

// cadre-server watches Deployments everywhere (an informer cannot be
// granted per namespace) but caches only its own; creating, updating and
// deleting them and their Services it may do only where
// config/rbac/workloads.yaml is bound, in each Team namespace. Services are
// read uncached. Owning them needs update on toolservers/finalizers, for
// blockOwnerDeletion where OwnerReferencesPermissionEnforcement is on.
//
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=cadre.dev,resources=toolservers/finalizers,verbs=update

// LabelToolServer labels a managed ToolServer's Deployment, its pods and
// its Service.
const LabelToolServer = "cadre.dev/toolserver"

// defaultMCPPath is a managed server's MCP endpoint path when it names
// none.
const defaultMCPPath = "/mcp"

// reconcileManaged runs a managed ToolServer: a Deployment of one replica
// with its image and port, and a ClusterIP Service in front of it, both
// named after it and owned by it. Its URL is the Service's; it is Ready once
// the Deployment's rollout of its current spec has completed. A Deployment
// or Service of its name that is not its own is left alone, and the
// ToolServer is not Ready. It is looked at again every resync: its Service
// is not watched.
func (r *toolServerReconciler) reconcileManaged(ctx context.Context, ts *v1alpha1.ToolServer) (reconcile.Result, error) {
	m := ts.Spec.Managed
	path := m.Path
	if path == "" {
		path = defaultMCPPath
	}
	url := fmt.Sprintf("http://%s.%s.svc:%d%s", ts.Name, ts.Namespace, m.Port, path)
	changed := ts.Status.URL != url
	ts.Status.URL = url
	again := reconcile.Result{RequeueAfter: r.resync}
	notReady := func(reason, msg string) (reconcile.Result, error) {
		return again, r.writeStatus(ctx, ts, &ts.Status.ConfigStatus, false, reason, msg, changed)
	}

	dep, err := r.ensureDeployment(ctx, ts)
	if err == nil {
		err = r.ensureService(ctx, ts)
	}
	var taken *nameTaken
	switch {
	case apierrors.IsForbidden(err):
		return notReady("Forbidden", forbiddenWorkloads)
	case errors.As(err, &taken):
		return notReady("NameTaken", taken.Error())
	case err != nil:
		return reconcile.Result{}, err
	}
	ready, reason, msg := rollout(dep)
	return again, r.writeStatus(ctx, ts, &ts.Status.ConfigStatus, ready, reason, msg, changed)
}

// forbiddenUnmanage says what a namespace where cadre-server may not delete
// a formerly managed ToolServer's Deployment or Service lacks.
const forbiddenUnmanage = "cadre-server may not delete the Deployment or Service this ToolServer ran as when managed: bind the ClusterRole in config/rbac/workloads.yaml here"

// forbiddenWorkloads says what a namespace where cadre-server may not run
// a managed ToolServer lacks.
const forbiddenWorkloads = "cadre-server may not run managed ToolServers in this namespace: bind the ClusterRole in config/rbac/workloads.yaml here"

// nameTaken is a Deployment or Service of a managed ToolServer's name that
// is not its own.
type nameTaken struct{ kind, name string }

func (e *nameTaken) Error() string {
	return fmt.Sprintf("a %s %s that is not this ToolServer's exists in the namespace", e.kind, e.name)
}

// rollout says whether dep runs its current spec on every replica, and if
// not why: still rolling out, or failed to.
func rollout(dep *appsv1.Deployment) (ready bool, reason, message string) {
	// Conditions are of the spec last seen: a failure of an earlier one is
	// not the current spec's.
	if dep.Status.ObservedGeneration < dep.Generation {
		return false, "Deploying", deploying
	}
	var available bool
	for _, c := range dep.Status.Conditions {
		switch {
		case c.Type == appsv1.DeploymentProgressing && c.Reason == "ProgressDeadlineExceeded":
			return false, "RolloutFailed", "its Deployment's rollout failed: " + c.Message
		case c.Type == appsv1.DeploymentAvailable:
			available = c.Status == corev1.ConditionTrue
		}
	}
	replicas := int32(1)
	if dep.Spec.Replicas != nil {
		replicas = *dep.Spec.Replicas
	}
	s := dep.Status
	if s.UpdatedReplicas != replicas || s.AvailableReplicas != replicas || !available {
		return false, "Deploying", deploying
	}
	return true, "Available", ""
}

const deploying = "its Deployment has not finished rolling out its current spec"

// ensureDeployment creates ts's Deployment, or brings its own to ts's spec.
func (r *toolServerReconciler) ensureDeployment(ctx context.Context, ts *v1alpha1.ToolServer) (*appsv1.Deployment, error) {
	labels := map[string]string{LabelToolServer: ts.Name}
	replicas := int32(1)
	ports := []corev1.ContainerPort{{Name: "mcp", ContainerPort: ts.Spec.Managed.Port, Protocol: corev1.ProtocolTCP}}
	var dep appsv1.Deployment
	err := r.client.Get(ctx, client.ObjectKey{Namespace: ts.Namespace, Name: ts.Name}, &dep)
	switch {
	case apierrors.IsNotFound(err):
		// The cache holds labelled Deployments only: one of the name
		// without the label shows up as AlreadyExists.
		dep = appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: ts.Name, Namespace: ts.Namespace, Labels: labels},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: ts.Spec.Managed.Image, Ports: ports}}},
				},
			},
		}
		if err := controllerutil.SetControllerReference(ts, &dep, r.client.Scheme()); err != nil {
			return nil, err
		}
		err := r.client.Create(ctx, &dep)
		if !apierrors.IsAlreadyExists(err) {
			return &dep, err
		}
		// Ours, created a moment ago and not in the cache yet, or
		// someone else's. Read into a fresh object: decoding into dep
		// would keep the owner and labels it was given here wherever
		// the one there has none.
		var there appsv1.Deployment
		if err := r.secrets.Get(ctx, client.ObjectKeyFromObject(&dep), &there); err != nil {
			return nil, err
		}
		dep = there
	case err != nil:
		return nil, err
	}
	if !metav1.IsControlledBy(&dep, ts) {
		return nil, &nameTaken{"Deployment", ts.Name}
	}
	// Only what Cadre sets is compared: the API server defaults the rest.
	c := &dep.Spec.Template.Spec.Containers[0]
	if dep.Spec.Replicas != nil && *dep.Spec.Replicas == replicas && c.Image == ts.Spec.Managed.Image && equality.Semantic.DeepEqual(c.Ports, ports) {
		return &dep, nil
	}
	dep.Spec.Replicas = &replicas
	c.Image, c.Ports = ts.Spec.Managed.Image, ports
	return &dep, r.client.Update(ctx, &dep)
}

// ensureService creates ts's Service, or brings its own to ts's spec. It is
// read uncached: cadre-server caches no Services.
func (r *toolServerReconciler) ensureService(ctx context.Context, ts *v1alpha1.ToolServer) error {
	selector := map[string]string{LabelToolServer: ts.Name}
	ports := []corev1.ServicePort{{Name: "mcp", Port: ts.Spec.Managed.Port, TargetPort: intstr.FromInt32(ts.Spec.Managed.Port), Protocol: corev1.ProtocolTCP}}
	var svc corev1.Service
	err := r.secrets.Get(ctx, client.ObjectKey{Namespace: ts.Namespace, Name: ts.Name}, &svc)
	switch {
	case apierrors.IsNotFound(err):
		svc = corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: ts.Name, Namespace: ts.Namespace, Labels: selector},
			Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: selector, Ports: ports},
		}
		if err := controllerutil.SetControllerReference(ts, &svc, r.client.Scheme()); err != nil {
			return err
		}
		return r.client.Create(ctx, &svc)
	case err != nil:
		return err
	}
	if !metav1.IsControlledBy(&svc, ts) {
		return &nameTaken{"Service", ts.Name}
	}
	if svc.Spec.Type == corev1.ServiceTypeClusterIP && equality.Semantic.DeepEqual(svc.Spec.Selector, selector) &&
		len(svc.Spec.Ports) == 1 && svc.Spec.Ports[0].Port == ports[0].Port && svc.Spec.Ports[0].TargetPort == ports[0].TargetPort {
		return nil
	}
	svc.Spec.Type, svc.Spec.Selector, svc.Spec.Ports = corev1.ServiceTypeClusterIP, selector, ports
	return r.client.Update(ctx, &svc)
}

// unmanage deletes the Deployment and Service a ToolServer no longer
// managed ran as, its own only, and forgets their URL. It reports whether
// the status changed. A Service it may not read it cannot have made; one
// it may not delete is a Forbidden, and the URL stays.
func (r *toolServerReconciler) unmanage(ctx context.Context, ts *v1alpha1.ToolServer) (bool, error) {
	key := client.ObjectKey{Namespace: ts.Namespace, Name: ts.Name}
	var dep appsv1.Deployment
	switch err := r.client.Get(ctx, key, &dep); {
	case err == nil && metav1.IsControlledBy(&dep, ts):
		if err := r.client.Delete(ctx, &dep, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return false, err
		}
	case client.IgnoreNotFound(err) != nil:
		return false, err
	}
	var svc corev1.Service
	switch err := r.secrets.Get(ctx, key, &svc); {
	case err == nil && metav1.IsControlledBy(&svc, ts):
		if err := r.client.Delete(ctx, &svc); client.IgnoreNotFound(err) != nil {
			return false, err
		}
	case client.IgnoreNotFound(err) != nil && !apierrors.IsForbidden(err):
		return false, err
	}
	changed := ts.Status.URL != ""
	ts.Status.URL = ""
	return changed, nil
}
