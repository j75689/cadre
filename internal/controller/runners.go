package controller

import (
	"context"
	"fmt"
	"strings"

	builderctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrl "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/token"
)

// +kubebuilder:rbac:groups=cadre.dev,resources=runners,verbs=get;list;watch
// +kubebuilder:rbac:groups=cadre.dev,resources=runners/status,verbs=get;update
// +kubebuilder:rbac:groups=cadre.dev,resources=runners/finalizers,verbs=update

type runnerReconciler struct{ base }

func (r *runnerReconciler) setup(mgr ctrl.Manager) error {
	return builderctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Runner{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// The Runners sharing a ServiceAccount decide which holds it: one
		// made, changed or deleted brings back the others.
		Watches(&v1alpha1.Runner{}, handler.EnqueueRequestsFromMapFunc(sharing(r.client, runnerSA, sharers)), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("runner").WithOptions(r.options()).Complete(r)
}

// sharers are the Runners in ns naming the ServiceAccount sa. Every reader
// that decides which holds it lists them here, and decides with holder,
// from their specs alone: no status, so no lag after an edit.
func sharers(ctx context.Context, c client.Reader, ns, sa string) ([]v1alpha1.Runner, error) {
	var list v1alpha1.RunnerList
	if err := c.List(ctx, &list, client.InNamespace(ns), client.MatchingFields{indexSA: sa}); err != nil {
		return nil, fmt.Errorf("list runners: %w", err)
	}
	return list.Items, nil
}

func (r *runnerReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var rn v1alpha1.Runner
	if err := r.client.Get(ctx, req.NamespacedName, &rn); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	// Before anything else: it mints nothing, and deletes nothing.
	if msg := invalidName("Runner", rn.Name); msg != "" {
		return reconcile.Result{RequeueAfter: r.resync}, r.setReady(ctx, &rn, &rn.Status.ConfigStatus, false, "InvalidName", msg)
	}
	if ok, err := r.hasTeam(ctx, req.Namespace); err != nil || !ok {
		return r.noTeam(ctx, &rn, &rn.Status.ConfigStatus, err)
	}
	if rn.Spec.ServiceAccount != nil {
		// A token minted before a switch to a ServiceAccount must not come
		// back to life when the Runner switches again.
		if err := r.deleteToken(ctx, r.tokenOwner(&rn)); err != nil {
			return reconcile.Result{}, err
		}
		changed := setToken(r.tokenOwner(&rn), "")
		// One ServiceAccount is one Runner's: a runner authenticated as it
		// claims with that Runner's labels, so a second Runner naming it
		// would take work it could never be given.
		list, err := sharers(ctx, r.client, rn.Namespace, rn.Spec.ServiceAccount.Name)
		if err != nil {
			return reconcile.Result{}, err
		}
		if h := holder(list); h != nil && h.Name != rn.Name {
			return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &rn, &rn.Status.ConfigStatus, false, "ServiceAccountInUse", inUse("Runner", rn.Spec.ServiceAccount.Name, h.Name), changed)
		}
		return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &rn, &rn.Status.ConfigStatus, true, "ServiceAccount", "", changed)
	}
	hash, problem, err := r.ensureToken(ctx, r.tokenOwner(&rn))
	if err != nil {
		return reconcile.Result{}, err
	}
	if problem != "" {
		// Whatever the Secret holds now, the old token is no longer what
		// it holds: refuse every token until the Secret is fixed.
		changed := setToken(r.tokenOwner(&rn), "")
		return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &rn, &rn.Status.ConfigStatus, false, "SecretNotOwned", problem, changed)
	}
	changed := setToken(r.tokenOwner(&rn), hash)
	return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &rn, &rn.Status.ConfigStatus, true, "TokenMinted", "", changed)
}

// tokenOwner is the Runner's minted token.
func (r *runnerReconciler) tokenOwner(rn *v1alpha1.Runner) tokenOwner {
	return tokenOwner{obj: rn, kind: "Runner", secret: TokenSecretName(rn.Name), label: v1alpha1.LabelRunner, status: &rn.Status.MintedToken}
}

// MintedIdentity is the identity of a runner authenticated by a minted
// token.
func MintedIdentity(namespace, runner string) string {
	return "cadre:runner:" + namespace + "/" + runner
}

// Runners authenticates runners and says where each may claim. A minted
// token (two parts) is checked against its Runner, read from the cache;
// its refusals are remembered for a moment, its acceptances never, so a
// rotated token stops as soon as the status changes. A ServiceAccount token
// (a JWT, three parts) goes to reviews, which may be cached; its Runners,
// those in the ServiceAccount's namespace naming it, are then found
// through an index, so a change to them applies at once, and one that no
// Runner holds is refused. builtin is cadre-runner's identity: it claims
// builtin agents everywhere and needs no Runner. c must be a controller manager's client, whose cache has the
// Runner index.
func Runners(c client.Client, reviews runner.Authenticator, builtin string) runner.Authenticator {
	a := runners{c: c, reviews: reviews, builtin: builtin}
	a.minted = runner.Refusals(mintedTokens{c: c})
	return a
}

type runners struct {
	c       client.Client
	reviews runner.Authenticator
	minted  runner.Authenticator
	builtin string
}

func (a runners) Authenticate(ctx context.Context, tok string) (runner.Identity, error) {
	if strings.Count(tok, ".") == 1 {
		return a.minted.Authenticate(ctx, tok)
	}
	id, err := a.reviews.Authenticate(ctx, tok)
	if err != nil || id.Name == a.builtin {
		return id, err
	}
	ns, name, ok := serviceAccountOf(id.Name)
	if !ok {
		return runner.Identity{}, runner.ErrUnauthenticated
	}
	list, err := sharers(ctx, a.c, ns, name)
	if err != nil {
		return runner.Identity{}, err
	}
	// The labels of the Runner holding the ServiceAccount, decided from the
	// specs as the Runner reconciler decides it: another Runner naming the
	// account gives nothing. A ServiceAccount no Runner holds, an
	// AgentAccess's say, is no runner.
	h := holder(list)
	if h == nil {
		return runner.Identity{}, runner.ErrUnauthenticated
	}
	id.Scope = map[string]map[string]string{h.Namespace: h.Spec.Labels}
	return id, nil
}

// mintedTokens checks a minted token. Every failure is ErrUnauthenticated,
// so a caller learns nothing about which part was wrong.
type mintedTokens struct{ c client.Reader }

func (m mintedTokens) Authenticate(ctx context.Context, tok string) (runner.Identity, error) {
	ns, name, ok := token.Locate(tok)
	if !ok {
		return runner.Identity{}, runner.ErrUnauthenticated
	}
	rn, err := checkMinted(ctx, m.c, "runner", ns, name, tok, runnerToken)
	if err != nil {
		return runner.Identity{}, err
	}
	return runner.Identity{Name: MintedIdentity(ns, name), Scope: map[string]map[string]string{ns: rn.Spec.Labels}}, nil
}

// runnerToken is a Runner's ServiceAccount and minted token.
func runnerToken(rn *v1alpha1.Runner) (*v1alpha1.ServiceAccountRef, *v1alpha1.MintedToken) {
	return rn.Spec.ServiceAccount, &rn.Status.MintedToken
}
