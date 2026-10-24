package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrl "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	builderctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/runner"
)

// +kubebuilder:rbac:groups=cadre.dev,resources=runners,verbs=get;list;watch
// +kubebuilder:rbac:groups=cadre.dev,resources=runners/status,verbs=get;update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=create

// A runner outside the cluster authenticates with a token Cadre mints:
// base64url("<namespace>/<runner>") + "." + base64url(32 random bytes), in
// the Secret <runner>-token, key token. The first part locates the Runner
// without an index; its status holds the token's SHA-256. The Secret is
// never watched: like every Secret the controllers touch it is read one at
// a time, on the resync. Deleting it rotates the token: the next reconcile
// mints a new one, and the old stops working when the hash changes.

const tokenKey = "token"

var enc = base64.RawURLEncoding

// TokenSecretName is the Secret a Runner's minted token is kept in.
func TokenSecretName(runner string) string { return runner + "-token" }

type runnerReconciler struct{ base }

func (r *runnerReconciler) setup(mgr ctrl.Manager) error {
	return builderctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Runner{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("runner").WithOptions(r.options()).Complete(r)
}

func (r *runnerReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var rn v1alpha1.Runner
	if err := r.client.Get(ctx, req.NamespacedName, &rn); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if ok, err := r.hasTeam(ctx, req.Namespace); err != nil || !ok {
		return noTeam(err, r.resync)
	}
	if rn.Spec.ServiceAccount != nil {
		changed := rn.Status.TokenHash != "" || rn.Status.TokenSecret != ""
		rn.Status.TokenSecret, rn.Status.TokenHash, rn.Status.MintedAt = "", "", nil
		return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &rn, &rn.Status.ConfigStatus, true, "ServiceAccount", "", changed)
	}
	hash, minted, err := r.ensureToken(ctx, &rn)
	if err != nil {
		return reconcile.Result{}, err
	}
	changed := rn.Status.TokenHash != hash || rn.Status.TokenSecret != TokenSecretName(rn.Name)
	if changed {
		rn.Status.TokenSecret, rn.Status.TokenHash = TokenSecretName(rn.Name), hash
		if minted || rn.Status.MintedAt == nil {
			rn.Status.MintedAt = &metav1.Time{Time: time.Now().UTC().Truncate(time.Second)}
		}
	}
	return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, &rn, &rn.Status.ConfigStatus, true, "TokenMinted", "", changed)
}

// ensureToken reads the Runner's token Secret and returns its token's
// hash, minting the Secret first if it does not exist. It is created
// before the status names its hash: a status write that is lost is redone
// from the Secret on the next reconcile.
func (r *runnerReconciler) ensureToken(ctx context.Context, rn *v1alpha1.Runner) (hash string, minted bool, err error) {
	key := client.ObjectKey{Namespace: rn.Namespace, Name: TokenSecretName(rn.Name)}
	var s corev1.Secret
	err = r.secrets.Get(ctx, key, &s)
	switch {
	case err == nil:
		tok := s.Data[tokenKey]
		if len(tok) == 0 || !ownedBy(&s, rn) {
			return "", false, fmt.Errorf("secret %s holds no token of runner %s; delete it to mint one", key, rn.Name)
		}
		return hashToken(string(tok)), false, nil
	case !apierrors.IsNotFound(err):
		return "", false, err
	}
	token, err := mintToken(rn.Namespace, rn.Name)
	if err != nil {
		return "", false, err
	}
	s = corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{v1alpha1.LabelRunner: rn.Name}},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{tokenKey: []byte(token)},
	}
	if err := controllerutil.SetControllerReference(rn, &s, r.client.Scheme()); err != nil {
		return "", false, err
	}
	if err := r.client.Create(ctx, &s); err != nil {
		return "", false, err // a concurrent create is redone on the requeue
	}
	return hashToken(token), true, nil
}

func ownedBy(s *corev1.Secret, rn *v1alpha1.Runner) bool {
	for _, o := range s.OwnerReferences {
		if o.UID == rn.UID {
			return true
		}
	}
	return false
}

func mintToken(namespace, name string) (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	return enc.EncodeToString([]byte(namespace+"/"+name)) + "." + enc.EncodeToString(secret), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// MintedIdentity is the identity of a runner authenticated by a minted
// token.
func MintedIdentity(namespace, runner string) string {
	return "cadre:runner:" + namespace + "/" + runner
}

// Runners authenticates runners and says where each may claim. A minted
// token (two parts) is checked against its Runner on a fresh read, and is
// never cached, so a rotated token stops at once. A ServiceAccount token
// (a JWT, three parts) goes to reviews, which may be cached; the
// identity's Runners, every one naming its ServiceAccount, are then
// listed from the cache, so a change to them applies at once. builtin is
// cadre-runner's identity: it claims builtin agents everywhere and needs
// no Runner.
func Runners(c client.Client, fresh client.Reader, reviews runner.Authenticator, builtin string) runner.Authenticator {
	return runners{c: c, fresh: fresh, reviews: reviews, builtin: builtin}
}

type runners struct {
	c       client.Client
	fresh   client.Reader
	reviews runner.Authenticator
	builtin string
}

func (a runners) Authenticate(ctx context.Context, token string) (runner.Identity, error) {
	if strings.Count(token, ".") == 1 {
		return a.minted(ctx, token)
	}
	id, err := a.reviews.Authenticate(ctx, token)
	if err != nil || id.Name == a.builtin {
		return id, err
	}
	var list v1alpha1.RunnerList
	if err := a.c.List(ctx, &list); err != nil {
		return runner.Identity{}, fmt.Errorf("list runners: %w", err)
	}
	id.Scope = map[string]map[string]string{}
	for _, rn := range list.Items {
		sa := rn.Spec.ServiceAccount
		if sa != nil && "system:serviceaccount:"+sa.Namespace+":"+sa.Name == id.Name {
			id.Scope[rn.Namespace] = rn.Spec.Labels
		}
	}
	return id, nil
}

// minted checks a minted token. Every failure is ErrUnauthenticated, so a
// caller learns nothing about which part was wrong.
func (a runners) minted(ctx context.Context, token string) (runner.Identity, error) {
	where, _, _ := strings.Cut(token, ".")
	raw, err := enc.DecodeString(where)
	if err != nil {
		return runner.Identity{}, runner.ErrUnauthenticated
	}
	ns, name, ok := strings.Cut(string(raw), "/")
	if !ok || len(validation.IsDNS1123Label(ns)) > 0 || len(validation.IsDNS1123Subdomain(name)) > 0 {
		return runner.Identity{}, runner.ErrUnauthenticated
	}
	var rn v1alpha1.Runner
	err = a.fresh.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &rn)
	switch {
	case apierrors.IsNotFound(err):
		return runner.Identity{}, runner.ErrUnauthenticated
	case err != nil:
		return runner.Identity{}, fmt.Errorf("read runner: %w", err)
	}
	if rn.Spec.ServiceAccount != nil || rn.Status.TokenHash == "" ||
		subtle.ConstantTimeCompare([]byte(rn.Status.TokenHash), []byte(hashToken(token))) != 1 {
		return runner.Identity{}, runner.ErrUnauthenticated
	}
	return runner.Identity{Name: MintedIdentity(ns, name), Scope: map[string]map[string]string{ns: rn.Spec.Labels}}, nil
}
