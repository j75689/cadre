package controller

import (
	"context"
	"fmt"

	builderctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrl "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

// +kubebuilder:rbac:groups=cadre.dev,resources=agentaccesses,verbs=get;list;watch
// +kubebuilder:rbac:groups=cadre.dev,resources=agentaccesses/status,verbs=get;update
// +kubebuilder:rbac:groups=cadre.dev,resources=agentaccesses/finalizers,verbs=update

// An AgentAccess is reconciled as a Runner is: a ServiceAccount is held by
// one AgentAccess in its namespace, and without one Cadre mints a token
// into <name>.access-token. A Runner's is <name>-token, and neither name
// has a dot, so the two never share a Secret. Its conditions are for
// humans: what a request is allowed is decided from the spec, and the
// minted token's hash, when it is made.

type agentAccessReconciler struct{ base }

func (r *agentAccessReconciler) setup(mgr ctrl.Manager) error {
	return builderctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.AgentAccess{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1alpha1.AgentAccess{}, handler.EnqueueRequestsFromMapFunc(sharing(r.client, accessSA, accessSharers)), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("agentaccess").WithOptions(r.options()).Complete(r)
}

// accessSharers are the AgentAccesses in ns naming the ServiceAccount sa,
// for holder to decide which holds it, from their specs alone.
func accessSharers(ctx context.Context, c client.Reader, ns, sa string) ([]v1alpha1.AgentAccess, error) {
	var list v1alpha1.AgentAccessList
	if err := c.List(ctx, &list, client.InNamespace(ns), client.MatchingFields{indexSA: sa}); err != nil {
		return nil, fmt.Errorf("list agent accesses: %w", err)
	}
	return list.Items, nil
}

// tokenOwner is the AgentAccess's minted token.
func (r *agentAccessReconciler) tokenOwner(aa *v1alpha1.AgentAccess) tokenOwner {
	return accessTokenOwner(aa)
}

// accessTokenOwner is aa's minted token, for the reconciler and the API's
// rotation alike.
func accessTokenOwner(aa *v1alpha1.AgentAccess) tokenOwner {
	return tokenOwner{obj: aa, kind: "AgentAccess", resource: "agentaccesses", secret: AccessTokenSecretName(aa.Name), label: v1alpha1.LabelAgentAccess, status: &aa.Status.MintedToken}
}

func (r *agentAccessReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var aa v1alpha1.AgentAccess
	if err := r.client.Get(ctx, req.NamespacedName, &aa); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	// Before anything else: it mints nothing, and deletes nothing.
	if msg := invalidName("AgentAccess", aa.Name); msg != "" {
		return reconcile.Result{RequeueAfter: r.resync}, r.setReady(ctx, &aa, &aa.Status.ConfigStatus, false, "InvalidName", msg)
	}
	if ok, err := r.hasTeam(ctx, req.Namespace); err != nil || !ok {
		return r.noTeam(ctx, &aa, &aa.Status.ConfigStatus, err)
	}
	if sa := aa.Spec.ServiceAccount; sa != nil {
		// A token minted before a switch to a ServiceAccount must not come
		// back to life when the AgentAccess switches again.
		if err := r.deleteToken(ctx, r.tokenOwner(&aa)); err != nil {
			return reconcile.Result{}, err
		}
		changed := setToken(r.tokenOwner(&aa), "")
		// One ServiceAccount is one AgentAccess's: an agent authenticated
		// as it acts as that AgentAccess, with its role.
		list, err := accessSharers(ctx, r.client, aa.Namespace, sa.Name)
		if err != nil {
			return reconcile.Result{}, err
		}
		if h := holder(list); h != nil && h.Name != aa.Name {
			return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &aa, &aa.Status.ConfigStatus, false, "ServiceAccountInUse", inUse("AgentAccess", sa.Name, h.Name), changed)
		}
		return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &aa, &aa.Status.ConfigStatus, true, "ServiceAccount", "", changed)
	}
	hash, problem, err := r.ensureToken(ctx, r.tokenOwner(&aa))
	if err != nil {
		return reconcile.Result{}, err
	}
	if problem != "" {
		// Whatever the Secret holds now, the old token is no longer what
		// it holds: refuse every token until the Secret is fixed.
		changed := setToken(r.tokenOwner(&aa), "")
		return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &aa, &aa.Status.ConfigStatus, false, "SecretNotOwned", problem, changed)
	}
	changed := setToken(r.tokenOwner(&aa), hash)
	return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &aa, &aa.Status.ConfigStatus, true, "TokenMinted", "", changed)
}
