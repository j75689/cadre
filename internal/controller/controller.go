// Package controller holds the reconcilers that turn Agent, ModelProvider,
// ToolServer and Runner resources into something the Task Manager can use:
// each gets a Ready condition saying whether it can be used, and a Runner
// outside the cluster gets a minted token.
//
// The reconcilers only act in namespaces that have a Team. They read Secrets
// one at a time, directly from the API server, and never watch or cache
// them, so cadre-server never holds the cluster's Secrets in memory; the
// resources that depend on a Secret are re-checked periodically instead.
// The one Secret they write is a Runner's minted token.
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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
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
	// ServerURL is where the pods of runner agents' Jobs reach cadre-server;
	// without it no Job is started.
	ServerURL string
	// Lease is the run lease: a Job still running a lease after its run
	// ended is deleted. Default 30 seconds.
	Lease time.Duration
}

// SetupOption adjusts Setup.
type SetupOption func(*setupConfig)

type setupConfig struct{ skipNameValidation bool }

// SkipNameValidation lets several managers register the same controllers in
// one process, as tests that run several cadre-servers do; controller-runtime
// otherwise rejects a second controller of a name.
func SkipNameValidation() SetupOption {
	return func(c *setupConfig) { c.skipNameValidation = true }
}

// Setup registers the reconcilers with mgr. Secrets are read through
// mgr.GetAPIReader, which bypasses the cache.
func Setup(mgr ctrl.Manager, o Options, opts ...SetupOption) error {
	var sc setupConfig
	for _, opt := range opts {
		opt(&sc)
	}
	if o.Resync <= 0 {
		o.Resync = DefaultResync
	}
	b := base{client: mgr.GetClient(), secrets: mgr.GetAPIReader(), resync: o.Resync, skipNames: sc.skipNameValidation}
	if err := setupIndexes(mgr); err != nil {
		return err
	}
	if err := (&modelProviderReconciler{b}).setup(mgr); err != nil {
		return err
	}
	if err := (&runnerReconciler{b}).setup(mgr); err != nil {
		return err
	}
	if err := (&toolServerReconciler{b}).setup(mgr); err != nil {
		return err
	}
	if o.ServerURL != "" {
		if o.Lease <= 0 {
			o.Lease = 30 * time.Second
		}
		jr := &jobReconciler{base: b, serverURL: o.ServerURL, lease: o.Lease,
			claimed: map[types.NamespacedName]time.Time{}, created: map[types.NamespacedName]map[string]time.Time{}}
		if err := jr.setup(mgr); err != nil {
			return err
		}
	}
	return (&agentReconciler{b}).setup(mgr)
}

// base is what every reconciler shares.
type base struct {
	client    client.Client
	secrets   client.Reader // uncached
	resync    time.Duration
	skipNames bool
}

// options returns the controller options every reconciler uses.
func (b base) options() controller.Options {
	return controller.Options{SkipNameValidation: &b.skipNames}
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
	return b.writeStatus(ctx, obj, status, ready, reason, message, false)
}

// writeStatus is setReady, writing also when changed says other status
// fields changed.
func (b base) writeStatus(ctx context.Context, obj client.Object, status *v1alpha1.ConfigStatus, ready bool, reason, message string, changed bool) error {
	cond := metav1.Condition{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reason, Message: message, ObservedGeneration: obj.GetGeneration()}
	if ready {
		cond.Status = metav1.ConditionTrue
	}
	changed = meta.SetStatusCondition(&status.Conditions, cond) || changed
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

// isReady reports whether obj is Ready for its current spec.
func isReady(obj client.Object, status v1alpha1.ConfigStatus) bool {
	return status.ReadyFor(obj.GetGeneration())
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
	// The Job informer holds runner agents' Jobs only: cadre-server may
	// list every Job, but keeps none of the others.
	jobs, err := labels.Parse(LabelAgent)
	if err != nil {
		return nil, err
	}
	return ctrl.New(cfg, ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cache.Options{ByObject: map[client.Object]cache.ByObject{&batchv1.Job{}: {Label: jobs}}},
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Client:                 client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
	})
}
