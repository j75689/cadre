package controller

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

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
// into <name>-access-token. A Runner and an AgentAccess never share a
// Secret: where their Secret names would coincide (Runner bot-access and
// AgentAccess bot) the Runner holds it, and the AgentAccess is not Ready
// (SecretNameTaken) until renamed. It yields without creating anything, so
// the two never race to mint. Its conditions are for humans: what a
// request is allowed is decided from the spec, and the minted token's
// hash, when it is made.

type agentAccessReconciler struct{ base }

func (r *agentAccessReconciler) setup(mgr ctrl.Manager) error {
	return builderctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.AgentAccess{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1alpha1.AgentAccess{}, handler.EnqueueRequestsFromMapFunc(sharing(r.client, accessSA, accessSharers)), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// A Runner whose Secret name is this one's, made or deleted.
		Watches(&v1alpha1.Runner{}, handler.EnqueueRequestsFromMapFunc(yieldsTo), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("agentaccess").WithOptions(r.options()).Complete(r)
}

// yieldsTo maps a Runner to the AgentAccess whose Secret name would be its
// own: Runner bot-access to AgentAccess bot.
func yieldsTo(_ context.Context, o client.Object) []reconcile.Request {
	name, ok := strings.CutSuffix(o.GetName(), "-access")
	if !ok || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: o.GetNamespace(), Name: name}}}
}

// runnerHolding is the Runner whose token Secret has the name the
// AgentAccess's would, if there is one: Runner bot-access, for bot.
func (r *agentAccessReconciler) runnerHolding(ctx context.Context, aa *v1alpha1.AgentAccess) (string, bool, error) {
	name := strings.TrimSuffix(AccessTokenSecretName(aa.Name), "-token")
	err := r.client.Get(ctx, client.ObjectKey{Namespace: aa.Namespace, Name: name}, &v1alpha1.Runner{})
	switch {
	case apierrors.IsNotFound(err):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return name, true, nil
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
	return tokenOwner{obj: aa, kind: "AgentAccess", secret: AccessTokenSecretName(aa.Name), label: v1alpha1.LabelAgentAccess, status: &aa.Status.MintedToken}
}

func (r *agentAccessReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var aa v1alpha1.AgentAccess
	if err := r.client.Get(ctx, req.NamespacedName, &aa); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
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
		changed := setToken(r.tokenOwner(&aa), "", false)
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
	// The Runner always wins a Secret name.
	switch rn, taken, err := r.runnerHolding(ctx, &aa); {
	case err != nil:
		return reconcile.Result{}, err
	case taken:
		if err := r.deleteToken(ctx, r.tokenOwner(&aa)); err != nil {
			return reconcile.Result{}, err
		}
		changed := setToken(r.tokenOwner(&aa), "", false)
		msg := fmt.Sprintf("Secret %s is Runner %s's token; give this AgentAccess another name", AccessTokenSecretName(aa.Name), rn)
		return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &aa, &aa.Status.ConfigStatus, false, "SecretNameTaken", msg, changed)
	}
	hash, minted, problem, err := r.ensureToken(ctx, r.tokenOwner(&aa))
	if err != nil {
		return reconcile.Result{}, err
	}
	if problem != "" {
		// Whatever the Secret holds now, the old token is no longer what
		// it holds: refuse every token until the Secret is fixed.
		changed := setToken(r.tokenOwner(&aa), "", false)
		return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &aa, &aa.Status.ConfigStatus, false, "SecretNotOwned", problem, changed)
	}
	changed := setToken(r.tokenOwner(&aa), hash, minted)
	return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &aa, &aa.Status.ConfigStatus, true, "TokenMinted", "", changed)
}
