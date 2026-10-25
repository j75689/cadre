package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/token"
)

// +kubebuilder:rbac:groups="",resources=secrets,verbs=create;delete

// An object without a ServiceAccount, a Runner or an AgentAccess,
// authenticates with a token Cadre mints (see internal/token) into a Secret
// of its own, <name>-token or <name>.access-token, key token; the object's
// status holds its hash. The Secret is never watched: like every Secret the
// controllers touch it is read one at a time, on the resync. Deleting it
// rotates the token: the next reconcile mints a new one, and the old stops
// working when the hash changes.

const tokenKey = "token"

// TokenSecretName is the Secret a Runner's minted token is kept in.
func TokenSecretName(runner string) string { return runner + "-token" }

// AccessTokenSecretName is the Secret an AgentAccess's minted token is kept
// in. It has a dot, which no Runner's or AgentAccess's name has, so it is
// never a Runner's.
func AccessTokenSecretName(access string) string { return access + ".access-token" }

// tokenOwner is an object Cadre mints a token for.
type tokenOwner struct {
	obj    client.Object
	kind   string // Runner, say: for messages
	secret string // the Secret's name
	label  string // the Secret's label naming obj
	status *v1alpha1.MintedToken
}

// setToken puts hash (none when empty) in the owner's status, and reports
// whether that changed it.
func setToken(o tokenOwner, hash string, minted bool) bool {
	st := o.status
	secret := ""
	if hash != "" {
		secret = o.secret
	}
	if st.TokenHash == hash && st.TokenSecret == secret {
		return false
	}
	st.TokenSecret, st.TokenHash = secret, hash
	switch {
	case hash == "":
		st.MintedAt = nil
	case minted || st.MintedAt == nil:
		st.MintedAt = &metav1.Time{Time: time.Now().UTC().Truncate(time.Second)}
	}
	return true
}

// ensureToken reads the owner's token Secret and returns its token's hash,
// minting the Secret first if it does not exist. It is created before the
// status names its hash: a status write that is lost is redone from the
// Secret on the next reconcile. A Secret of that name the owner does not
// own, or one without a token, is a problem, not a token.
func (b base) ensureToken(ctx context.Context, o tokenOwner) (hash string, minted bool, problem string, err error) {
	key := client.ObjectKey{Namespace: o.obj.GetNamespace(), Name: o.secret}
	var s corev1.Secret
	err = b.secrets.Get(ctx, key, &s)
	switch {
	case err == nil:
		tok := s.Data[tokenKey]
		if len(tok) == 0 || !ownedBy(&s, o.obj) {
			return "", false, fmt.Sprintf("Secret %s exists but is not this %s's token; delete it to have a token minted", key.Name, o.kind), nil
		}
		return token.Hash(string(tok)), false, "", nil
	case !apierrors.IsNotFound(err):
		return "", false, "", err
	}
	tok, hash, err := token.Mint(key.Namespace, o.obj.GetName())
	if err != nil {
		return "", false, "", err
	}
	s = corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{o.label: o.obj.GetName()}},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{tokenKey: []byte(tok)},
	}
	if err := controllerutil.SetControllerReference(o.obj, &s, b.client.Scheme()); err != nil {
		return "", false, "", err
	}
	if err := b.client.Create(ctx, &s); err != nil {
		return "", false, "", err // a concurrent create is redone on the requeue
	}
	return hash, true, "", nil
}

// invalidName is why name cannot be a Runner's or an AgentAccess's (kind),
// or "": it names and labels their token Secret, so it is a DNS label. The
// CRDs refuse other names at create; one made before that is told.
func invalidName(kind, name string) string {
	if len(validation.IsDNS1123Label(name)) == 0 {
		return ""
	}
	return fmt.Sprintf("%s names are DNS labels (at most 63 characters, no dot); re-create this %s under another name", kind, kind)
}

// deleteToken deletes the owner's own token Secret, if it has one.
func (b base) deleteToken(ctx context.Context, o tokenOwner) error {
	var s corev1.Secret
	err := b.secrets.Get(ctx, client.ObjectKey{Namespace: o.obj.GetNamespace(), Name: o.secret}, &s)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case !ownedBy(&s, o.obj):
		return nil // not ours to delete
	}
	return client.IgnoreNotFound(b.client.Delete(ctx, &s, client.Preconditions{UID: &s.UID}))
}

func ownedBy(s *corev1.Secret, obj client.Object) bool {
	for _, o := range s.OwnerReferences {
		if o.UID == obj.GetUID() {
			return true
		}
	}
	return false
}
