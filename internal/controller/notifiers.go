package controller

import (
	"context"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
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
// +kubebuilder:rbac:groups=cadre.dev,resources=notifiers/status,verbs=get;update

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
	again := reconcile.Result{RequeueAfter: r.resync}
	if ok, err := r.hasTeam(ctx, req.Namespace); err != nil {
		return reconcile.Result{}, err
	} else if !ok {
		return again, r.setReady(ctx, &n, &n.Status.ConfigStatus, false, "NoTeam", "namespace "+req.Namespace+" has no Team")
	}
	if u, err := url.Parse(n.Spec.Webhook.URL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return again, r.setReady(ctx, &n, &n.Status.ConfigStatus, false, "InvalidURL", "webhook URL "+n.Spec.Webhook.URL+" is not an http or https URL")
	}
	if ref := n.Spec.Webhook.SecretRef; ref != nil {
		ok, _, msg, err := r.secretHasKey(ctx, n.Namespace, *ref)
		if err != nil {
			return reconcile.Result{}, err
		}
		if !ok {
			return again, r.setReady(ctx, &n, &n.Status.ConfigStatus, false, "SecretMissing", msg)
		}
	}
	return again, r.setReady(ctx, &n, &n.Status.ConfigStatus, true, "Ready", "")
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

// Delivered writes lastSent or lastError, on a fresh copy, retried on a
// conflict and otherwise given up: it is a record, not a decision.
func (n notifiers) Delivered(ctx context.Context, nf *v1alpha1.Notifier, at time.Time, err error) {
	_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Notifier
		if gerr := n.fresh.Get(ctx, client.ObjectKeyFromObject(nf), &cur); gerr != nil {
			return gerr
		}
		t := metav1.NewTime(at)
		if err == nil {
			cur.Status.LastSent = &t
		} else {
			cur.Status.LastError = &v1alpha1.DeliveryError{Message: err.Error(), At: t}
		}
		return n.client.Status().Update(ctx, &cur)
	})
}

func (n notifiers) Warn(_ context.Context, t *v1alpha1.Task, reason, message string) {
	n.events.Eventf(t, nil, corev1.EventTypeWarning, reason, "Notify", "%s", message)
}
