package controller

import (
	"context"
	"encoding/json"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/manager"
)

// The Task Manager delivers notifications; the reconciler only says
// whether a Notifier can be delivered to.
//
// +kubebuilder:rbac:groups=cadre.dev,resources=notifiers,verbs=get;list;watch
// +kubebuilder:rbac:groups=cadre.dev,resources=notifiers/status,verbs=get;update;patch

type notifierReconciler struct{ base }

func (r *notifierReconciler) setup(mgr ctrl.Manager) error {
	// Deliveries write the status too; only spec changes and the periodic
	// re-check of the Secret bring a Notifier back.
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Notifier{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("notifier").WithOptions(r.options()).Complete(r)
}

func (r *notifierReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var n v1alpha1.Notifier
	if err := r.client.Get(ctx, req.NamespacedName, &n); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	ready, reason, msg, err := r.decide(ctx, &n)
	if err != nil {
		return reconcile.Result{}, err
	}
	// Deliveries write this status too: a write that loses to one is
	// tried again at once, as only a spec change would bring the
	// Notifier back otherwise.
	switch err := r.writeStatusOrConflict(ctx, &n, &n.Status.ConfigStatus, ready, reason, msg, false); {
	case apierrors.IsConflict(err):
		return reconcile.Result{RequeueAfter: 100 * time.Millisecond}, nil
	case err != nil:
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: r.resync}, nil
}

// decide says whether n can be delivered to, and if not why.
func (r *notifierReconciler) decide(ctx context.Context, n *v1alpha1.Notifier) (bool, string, string, error) {
	if ok, err := r.hasTeam(ctx, n.Namespace); err != nil || !ok {
		return false, "NoTeam", "namespace " + n.Namespace + " has no Team", err
	}
	u, err := url.Parse(n.Spec.Webhook.URL)
	switch {
	case err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https"):
		return false, "InvalidURL", "the webhook URL is not an http or https URL", nil
	case u.User != nil:
		// It would be sent as an Authorization header, which the spec
		// refuses: credentials go in the Secret.
		return false, "InvalidURL", "the webhook URL holds credentials: sign deliveries with secretRef instead", nil
	}
	if ref := n.Spec.Webhook.SecretRef; ref != nil {
		ok, _, msg, err := r.secretHasKey(ctx, n.Namespace, *ref)
		if err != nil || !ok {
			return false, "SecretMissing", msg, err
		}
	}
	return true, "Ready", "", nil
}

// Notifiers gives the Task Manager cached reads of Notifiers, fresh reads
// of their signing Secrets, their delivery status, and Events on Tasks.
func Notifiers(mgr ctrl.Manager) manager.Notifiers {
	return notifiers{agents: sources(mgr), client: mgr.GetClient(), events: mgr.GetEventRecorder("cadre-notifier")}
}

type notifiers struct {
	agents
	client client.Client
	events events.EventRecorder
}

func (n notifiers) ListNotifiers(ctx context.Context, ns string) ([]v1alpha1.Notifier, error) {
	var list v1alpha1.NotifierList
	if err := n.cached.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// Delivered writes lastSent or lastError as a merge patch of the status,
// which cannot conflict with the reconciler's conditions; the same error
// again is not written. It is a record, not a decision: a failure is given
// up.
func (n notifiers) Delivered(ctx context.Context, nf *v1alpha1.Notifier, at time.Time, err error) {
	if err != nil && nf.Status.LastError != nil && nf.Status.LastError.Message == err.Error() {
		return
	}
	t := metav1.NewTime(at)
	status := map[string]any{"lastSent": t}
	if err != nil {
		status = map[string]any{"lastError": v1alpha1.DeliveryError{Message: err.Error(), At: t}}
	}
	data, merr := json.Marshal(map[string]any{"status": status})
	if merr != nil {
		return
	}
	_ = n.client.Status().Patch(ctx, nf.DeepCopy(), client.RawPatch(types.MergePatchType, data))
}

func (n notifiers) Warn(_ context.Context, t *v1alpha1.Task, reason, message string) {
	n.events.Eventf(t, nil, corev1.EventTypeWarning, reason, "Notify", "%s", message)
}
