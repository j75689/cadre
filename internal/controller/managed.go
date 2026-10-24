package controller

import (
	"context"
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
// read uncached.
//
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

// LabelToolServer labels a managed ToolServer's Deployment, its pods and
// its Service.
const LabelToolServer = "cadre.dev/toolserver"

// defaultMCPPath is a managed server's MCP endpoint path when it names
// none.
const defaultMCPPath = "/mcp"

// reconcileManaged runs a managed ToolServer: a Deployment of one replica
// with its image and port, and a ClusterIP Service in front of it, both
// named after it and owned by it. Its URL is the Service's; it is Ready once
// the Deployment is Available for its current spec.
func (r *toolServerReconciler) reconcileManaged(ctx context.Context, ts *v1alpha1.ToolServer) (reconcile.Result, error) {
	m := ts.Spec.Managed
	path := m.Path
	if path == "" {
		path = defaultMCPPath
	}
	url := fmt.Sprintf("http://%s.%s.svc:%d%s", ts.Name, ts.Namespace, m.Port, path)
	changed := ts.Status.URL != url
	ts.Status.URL = url

	dep, err := r.ensureDeployment(ctx, ts)
	switch {
	case apierrors.IsForbidden(err):
		return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, ts, &ts.Status.ConfigStatus, false, "Forbidden", forbiddenWorkloads, changed)
	case apierrors.IsAlreadyExists(err):
		msg := fmt.Sprintf("a Deployment %s not Cadre's exists in the namespace", ts.Name)
		return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, ts, &ts.Status.ConfigStatus, false, "NameTaken", msg, changed)
	case err != nil:
		return reconcile.Result{}, err
	}
	switch err := r.ensureService(ctx, ts); {
	case apierrors.IsForbidden(err):
		return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, ts, &ts.Status.ConfigStatus, false, "Forbidden", forbiddenWorkloads, changed)
	case err != nil:
		return reconcile.Result{}, err
	}
	if !available(dep) {
		return reconcile.Result{}, r.writeStatus(ctx, ts, &ts.Status.ConfigStatus, false, "Deploying", "its Deployment is not Available yet", changed)
	}
	return reconcile.Result{}, r.writeStatus(ctx, ts, &ts.Status.ConfigStatus, true, "Available", "", changed)
}

// forbiddenWorkloads says what a namespace where cadre-server may not run
// a managed ToolServer lacks.
const forbiddenWorkloads = "cadre-server may not run managed ToolServers in this namespace: bind the ClusterRole in config/rbac/workloads.yaml here"

// available reports whether dep runs its current spec and is Available.
func available(dep *appsv1.Deployment) bool {
	if dep.Status.ObservedGeneration < dep.Generation {
		return false
	}
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// ensureDeployment creates ts's Deployment, or brings it to ts's spec.
func (r *toolServerReconciler) ensureDeployment(ctx context.Context, ts *v1alpha1.ToolServer) (*appsv1.Deployment, error) {
	labels := map[string]string{LabelToolServer: ts.Name}
	replicas := int32(1)
	want := appsv1.DeploymentSpec{
		Replicas: &replicas,
		Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "server", Image: ts.Spec.Managed.Image,
				Ports: []corev1.ContainerPort{{Name: "mcp", ContainerPort: ts.Spec.Managed.Port, Protocol: corev1.ProtocolTCP}},
			}}},
		},
	}
	var dep appsv1.Deployment
	err := r.client.Get(ctx, client.ObjectKey{Namespace: ts.Namespace, Name: ts.Name}, &dep)
	switch {
	case apierrors.IsNotFound(err):
		dep = appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ts.Name, Namespace: ts.Namespace, Labels: labels}, Spec: want}
		if err := controllerutil.SetControllerReference(ts, &dep, r.client.Scheme()); err != nil {
			return nil, err
		}
		err := r.client.Create(ctx, &dep)
		if apierrors.IsAlreadyExists(err) {
			// Ours, created a moment ago and not in the cache yet, or
			// someone else's.
			var there appsv1.Deployment
			if gerr := r.secrets.Get(ctx, client.ObjectKeyFromObject(&dep), &there); gerr == nil && metav1.IsControlledBy(&there, ts) {
				return &there, nil
			}
		}
		return &dep, err
	case err != nil:
		return nil, err
	}
	// Only what Cadre sets is compared: the API server defaults the rest.
	c := &dep.Spec.Template.Spec.Containers[0]
	if *dep.Spec.Replicas == replicas && c.Image == ts.Spec.Managed.Image && equality.Semantic.DeepEqual(c.Ports, want.Template.Spec.Containers[0].Ports) {
		return &dep, nil
	}
	dep.Spec.Replicas = &replicas
	c.Image, c.Ports = ts.Spec.Managed.Image, want.Template.Spec.Containers[0].Ports
	return &dep, r.client.Update(ctx, &dep)
}

// ensureService creates ts's Service, or brings its port to ts's spec. It
// is read uncached: cadre-server caches no Services.
func (r *toolServerReconciler) ensureService(ctx context.Context, ts *v1alpha1.ToolServer) error {
	port := corev1.ServicePort{Name: "mcp", Port: ts.Spec.Managed.Port, TargetPort: intstr.FromInt32(ts.Spec.Managed.Port), Protocol: corev1.ProtocolTCP}
	var svc corev1.Service
	err := r.secrets.Get(ctx, client.ObjectKey{Namespace: ts.Namespace, Name: ts.Name}, &svc)
	switch {
	case apierrors.IsNotFound(err):
		svc = corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: ts.Name, Namespace: ts.Namespace, Labels: map[string]string{LabelToolServer: ts.Name}},
			Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{LabelToolServer: ts.Name}, Ports: []corev1.ServicePort{port}},
		}
		if err := controllerutil.SetControllerReference(ts, &svc, r.client.Scheme()); err != nil {
			return err
		}
		return r.client.Create(ctx, &svc)
	case err != nil:
		return err
	}
	if len(svc.Spec.Ports) == 1 && svc.Spec.Ports[0].Port == port.Port && svc.Spec.Ports[0].TargetPort == port.TargetPort {
		return nil
	}
	svc.Spec.Ports = []corev1.ServicePort{port}
	return r.client.Update(ctx, &svc)
}
