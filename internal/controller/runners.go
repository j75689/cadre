package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	builderctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
// +kubebuilder:rbac:groups="",resources=secrets,verbs=create;delete

// A runner outside the cluster authenticates with a token Cadre mints (see
// internal/token) into the Secret <runner>-token, key token; the Runner's
// status holds its hash. The Secret is never watched: like every Secret the
// controllers touch it is read one at a time, on the resync. Deleting it
// rotates the token: the next reconcile mints a new one, and the old stops
// working when the hash changes.

const tokenKey = "token"

// TokenSecretName is the Secret a Runner's minted token is kept in.
func TokenSecretName(runner string) string { return runner + "-token" }

type runnerReconciler struct{ base }

func (r *runnerReconciler) setup(mgr ctrl.Manager) error {
	return builderctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Runner{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// The Runners sharing a ServiceAccount decide which holds it: one
		// made, changed or deleted brings back the others.
		Watches(&v1alpha1.Runner{}, handler.EnqueueRequestsFromMapFunc(r.sharing), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("runner").WithOptions(r.options()).Complete(r)
}

// sharing maps a Runner to the others in its namespace naming the same
// ServiceAccount.
func (r *runnerReconciler) sharing(ctx context.Context, o client.Object) []reconcile.Request {
	rn, ok := o.(*v1alpha1.Runner)
	if !ok || rn.Spec.ServiceAccount == nil {
		return nil
	}
	others, err := sharers(ctx, r.client, rn.Namespace, rn.Spec.ServiceAccount.Name)
	if err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, other := range others {
		if other.Name != rn.Name {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&other)})
		}
	}
	return reqs
}

// sharers are the Runners in ns naming the ServiceAccount sa. Every reader
// that decides which holds it lists them here, and decides with holder,
// from their specs alone: no status, so no lag after an edit.
func sharers(ctx context.Context, c client.Reader, ns, sa string) ([]v1alpha1.Runner, error) {
	var list v1alpha1.RunnerList
	if err := c.List(ctx, &list, client.InNamespace(ns), client.MatchingFields{indexRunnerSA: sa}); err != nil {
		return nil, fmt.Errorf("list runners: %w", err)
	}
	return list.Items, nil
}

// inUse is the message of a Runner, and of an Agent on it, whose
// ServiceAccount another Runner holds.
func inUse(sa, holder string) string {
	return fmt.Sprintf("ServiceAccount %s is Runner %s's, the oldest Runner naming it (then by name); give this Runner a ServiceAccount of its own", sa, holder)
}

// holder is the Runner of runners that holds their ServiceAccount: the
// oldest, then by name, so that every reader picks the same one.
func holder(runners []v1alpha1.Runner) *v1alpha1.Runner {
	if len(runners) == 0 {
		return nil
	}
	h := slices.MinFunc(runners, func(a, b v1alpha1.Runner) int {
		return cmp.Or(a.CreationTimestamp.Compare(b.CreationTimestamp.Time), strings.Compare(a.Name, b.Name))
	})
	return &h
}

func (r *runnerReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var rn v1alpha1.Runner
	if err := r.client.Get(ctx, req.NamespacedName, &rn); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if ok, err := r.hasTeam(ctx, req.Namespace); err != nil || !ok {
		return r.noTeam(ctx, &rn, &rn.Status.ConfigStatus, err)
	}
	if rn.Spec.ServiceAccount != nil {
		// A token minted before a switch to a ServiceAccount must not come
		// back to life when the Runner switches again.
		if err := r.deleteToken(ctx, &rn); err != nil {
			return reconcile.Result{}, err
		}
		changed := r.setToken(&rn, "", false)
		// One ServiceAccount is one Runner's: a runner authenticated as it
		// claims with that Runner's labels, so a second Runner naming it
		// would take work it could never be given.
		list, err := sharers(ctx, r.client, rn.Namespace, rn.Spec.ServiceAccount.Name)
		if err != nil {
			return reconcile.Result{}, err
		}
		if h := holder(list); h != nil && h.Name != rn.Name {
			return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &rn, &rn.Status.ConfigStatus, false, "ServiceAccountInUse", inUse(rn.Spec.ServiceAccount.Name, h.Name), changed)
		}
		return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &rn, &rn.Status.ConfigStatus, true, "ServiceAccount", "", changed)
	}
	hash, minted, problem, err := r.ensureToken(ctx, &rn)
	if err != nil {
		return reconcile.Result{}, err
	}
	if problem != "" {
		// Whatever the Secret holds now, the old token is no longer what
		// it holds: refuse every token until the Secret is fixed.
		changed := r.setToken(&rn, "", false)
		return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &rn, &rn.Status.ConfigStatus, false, "SecretNotOwned", problem, changed)
	}
	changed := r.setToken(&rn, hash, minted)
	return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &rn, &rn.Status.ConfigStatus, true, "TokenMinted", "", changed)
}

// setToken puts hash (none when empty) in the Runner's status, and reports
// whether that changed it.
func (r *runnerReconciler) setToken(rn *v1alpha1.Runner, hash string, minted bool) bool {
	secret := ""
	if hash != "" {
		secret = TokenSecretName(rn.Name)
	}
	if rn.Status.TokenHash == hash && rn.Status.TokenSecret == secret {
		return false
	}
	rn.Status.TokenSecret, rn.Status.TokenHash = secret, hash
	switch {
	case hash == "":
		rn.Status.MintedAt = nil
	case minted || rn.Status.MintedAt == nil:
		rn.Status.MintedAt = &metav1.Time{Time: time.Now().UTC().Truncate(time.Second)}
	}
	return true
}

// ensureToken reads the Runner's token Secret and returns its token's
// hash, minting the Secret first if it does not exist. It is created
// before the status names its hash: a status write that is lost is redone
// from the Secret on the next reconcile. A Secret of that name the Runner
// does not own, or one without a token, is a problem, not a token.
func (r *runnerReconciler) ensureToken(ctx context.Context, rn *v1alpha1.Runner) (hash string, minted bool, problem string, err error) {
	key := client.ObjectKey{Namespace: rn.Namespace, Name: TokenSecretName(rn.Name)}
	var s corev1.Secret
	err = r.secrets.Get(ctx, key, &s)
	switch {
	case err == nil:
		tok := s.Data[tokenKey]
		if len(tok) == 0 || !ownedBy(&s, rn) {
			return "", false, fmt.Sprintf("Secret %s exists but is not this Runner's token; delete it to have a token minted", key.Name), nil
		}
		return token.Hash(string(tok)), false, "", nil
	case !apierrors.IsNotFound(err):
		return "", false, "", err
	}
	tok, hash, err := token.Mint(rn.Namespace, rn.Name)
	if err != nil {
		return "", false, "", err
	}
	s = corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{v1alpha1.LabelRunner: rn.Name}},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{tokenKey: []byte(tok)},
	}
	if err := controllerutil.SetControllerReference(rn, &s, r.client.Scheme()); err != nil {
		return "", false, "", err
	}
	if err := r.client.Create(ctx, &s); err != nil {
		return "", false, "", err // a concurrent create is redone on the requeue
	}
	return hash, true, "", nil
}

// deleteToken deletes the Runner's own token Secret, if it has one.
func (r *runnerReconciler) deleteToken(ctx context.Context, rn *v1alpha1.Runner) error {
	var s corev1.Secret
	err := r.secrets.Get(ctx, client.ObjectKey{Namespace: rn.Namespace, Name: TokenSecretName(rn.Name)}, &s)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case !ownedBy(&s, rn):
		return nil // not ours to delete
	}
	return client.IgnoreNotFound(r.client.Delete(ctx, &s, client.Preconditions{UID: &s.UID}))
}

func ownedBy(s *corev1.Secret, rn *v1alpha1.Runner) bool {
	for _, o := range s.OwnerReferences {
		if o.UID == rn.UID {
			return true
		}
	}
	return false
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
// through an index, so a change to them applies at once. builtin is
// cadre-runner's identity: it claims builtin agents everywhere and needs no
// Runner. c must be a controller manager's client, whose cache has the
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

const saPrefix = "system:serviceaccount:"

func (a runners) Authenticate(ctx context.Context, tok string) (runner.Identity, error) {
	if strings.Count(tok, ".") == 1 {
		return a.minted.Authenticate(ctx, tok)
	}
	id, err := a.reviews.Authenticate(ctx, tok)
	if err != nil || id.Name == a.builtin {
		return id, err
	}
	ns, name, ok := strings.Cut(strings.TrimPrefix(id.Name, saPrefix), ":")
	if !ok || !strings.HasPrefix(id.Name, saPrefix) {
		return runner.Identity{}, runner.ErrUnauthenticated
	}
	list, err := sharers(ctx, a.c, ns, name)
	if err != nil {
		return runner.Identity{}, err
	}
	// The labels of the Runner holding the ServiceAccount, decided from the
	// specs as the Runner reconciler decides it: another Runner naming the
	// account gives nothing.
	id.Scope = map[string]map[string]string{}
	if h := holder(list); h != nil {
		id.Scope[h.Namespace] = h.Spec.Labels
	}
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
	var rn v1alpha1.Runner
	err := m.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &rn)
	switch {
	case apierrors.IsNotFound(err):
		return runner.Identity{}, runner.ErrUnauthenticated
	case err != nil:
		return runner.Identity{}, fmt.Errorf("read runner: %w", err)
	}
	if rn.Spec.ServiceAccount != nil || !token.Matches(tok, rn.Status.TokenHash) {
		return runner.Identity{}, runner.ErrUnauthenticated
	}
	return runner.Identity{Name: MintedIdentity(ns, name), Scope: map[string]map[string]string{ns: rn.Spec.Labels}}, nil
}
