// Package controller holds the reconcilers that turn Agent, ModelProvider and
// ToolServer resources into something the Task Manager can use: each gets a
// Ready condition saying whether it can be used.
//
// The reconcilers only act in namespaces that have a Team. They read Secrets
// one at a time, directly from the API server, and never watch or cache
// them, so cadre-server never holds the cluster's Secrets in memory; the
// resources that depend on a Secret are re-checked periodically instead.
//
// Leader election is off: every status write is a conditional update, so two
// cadre-server replicas reconciling the same object only race harmlessly. A
// Helm chart may later enable leader election for the controllers alone.
//
// +kubebuilder:rbac:groups=cadre.dev,resources=teams;agents;modelproviders;toolservers,verbs=get;list;watch
// +kubebuilder:rbac:groups=cadre.dev,resources=agents/status;modelproviders/status;toolservers/status,verbs=get;update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

// DefaultResync is how often resources that depend on a Secret are checked
// again.
const DefaultResync = time.Minute

// Options configures the reconcilers.
type Options struct {
	// Resync is how often Secret-dependent resources are re-checked.
	Resync time.Duration
}

// Setup registers the reconcilers with mgr. Secrets are read through
// mgr.GetAPIReader, which bypasses the cache.
func Setup(mgr ctrl.Manager, o Options) error {
	if o.Resync <= 0 {
		o.Resync = DefaultResync
	}
	b := base{client: mgr.GetClient(), secrets: mgr.GetAPIReader(), resync: o.Resync}
	if err := setupIndexes(mgr); err != nil {
		return err
	}
	if err := (&modelProviderReconciler{b}).setup(mgr); err != nil {
		return err
	}
	if err := (&toolServerReconciler{b}).setup(mgr); err != nil {
		return err
	}
	return (&agentReconciler{b}).setup(mgr)
}

// base is what every reconciler shares.
type base struct {
	client  client.Client
	secrets client.Reader // uncached
	resync  time.Duration
}

// hasTeam reports whether ns is a Team namespace.
func (b base) hasTeam(ctx context.Context, ns string) (bool, error) {
	err := b.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: v1alpha1.TeamName}, &v1alpha1.Team{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// secretHasKey reads one Secret directly and reports whether it holds key.
// The value is never kept or logged.
func (b base) secretHasKey(ctx context.Context, ns string, ref v1alpha1.SecretKeyRef) (ready bool, reason, message string, err error) {
	var s corev1.Secret
	err = b.secrets.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &s)
	switch {
	case apierrors.IsNotFound(err):
		return false, "SecretNotFound", "Secret " + ref.Name + " does not exist", nil
	case err != nil:
		return false, "", "", err
	}
	if _, ok := s.Data[ref.Key]; !ok {
		return false, "SecretKeyNotFound", "Secret " + ref.Name + " has no key " + ref.Key, nil
	}
	return true, "SecretFound", "", nil
}

// setReady writes the Ready condition and observedGeneration with a
// conditional update, and only when they change.
func (b base) setReady(ctx context.Context, obj client.Object, status *v1alpha1.ConfigStatus, ready bool, reason, message string) error {
	cond := metav1.Condition{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reason, Message: message, ObservedGeneration: obj.GetGeneration()}
	if ready {
		cond.Status = metav1.ConditionTrue
	}
	changed := meta.SetStatusCondition(&status.Conditions, cond)
	if status.ObservedGeneration != obj.GetGeneration() {
		status.ObservedGeneration = obj.GetGeneration()
		changed = true
	}
	if !changed {
		return nil
	}
	err := b.client.Status().Update(ctx, obj)
	if apierrors.IsConflict(err) {
		return nil // someone else wrote it; the watch brings us back
	}
	return err
}

// isReady reports whether obj is Ready for its current spec: a Ready
// condition decided for an older generation does not count.
func isReady(obj client.Object, status v1alpha1.ConfigStatus) bool {
	c := meta.FindStatusCondition(status.Conditions, v1alpha1.ConditionReady)
	gen := obj.GetGeneration()
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == gen && status.ObservedGeneration == gen
}

// NewManager returns a controller manager for cadre-server: no metrics or
// health endpoints of its own, no leader election, and Secrets kept out of
// the cache even if a client asks for one.
func NewManager(cfg *rest.Config) (ctrl.Manager, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return ctrl.New(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Client:                 client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
	})
}
