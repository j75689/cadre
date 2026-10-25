package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/token"
)

// +kubebuilder:rbac:groups="",resources=secrets,verbs=create;update;delete

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
// in: v1alpha1's, which the CLI names too.
func AccessTokenSecretName(access string) string { return v1alpha1.AccessTokenSecretName(access) }

// tokenOwner is an object Cadre mints a token for.
type tokenOwner struct {
	obj    client.Object
	kind   string // Runner, say: for messages
	secret string // the Secret's name
	label  string // the Secret's label naming obj
	status *v1alpha1.MintedToken
}

// setToken puts hash (none when empty) in the owner's status, and reports
// whether that changed it. A new hash is a new token, minted or written
// into the Secret by hand: mintedAt says when the status first had it.
func setToken(o tokenOwner, hash string) bool {
	st := o.status
	secret := ""
	if hash != "" {
		secret = o.secret
	}
	if st.TokenHash == hash && st.TokenSecret == secret {
		return false
	}
	if hash == "" {
		st.MintedAt = nil
	} else if st.TokenHash != hash {
		st.MintedAt = &metav1.Time{Time: time.Now().UTC().Truncate(time.Second)}
	}
	st.TokenSecret, st.TokenHash = secret, hash
	return true
}

// ensureToken reads the owner's token Secret and returns its token's hash,
// minting the Secret first if it does not exist. It is created before the
// status names its hash: a status write that is lost is redone from the
// Secret on the next reconcile. A Secret of that name the owner does not
// own, or one without a token, is a problem, not a token.
func (b base) ensureToken(ctx context.Context, o tokenOwner) (hash, problem string, err error) {
	sec, err := ownedSecret(ctx, b.secrets, o)
	var notOwned *notOwnedError
	switch {
	case errors.As(err, &notOwned):
		return "", notOwned.problem() + "; delete it to have a token minted", nil
	case err != nil:
		return "", "", err
	case sec != nil:
		return token.Hash(string(sec.Data[tokenKey])), "", nil
	}
	tok, hash, err := token.Mint(o.obj.GetNamespace(), o.obj.GetName())
	if err != nil {
		return "", "", err
	}
	if err := createToken(ctx, b.client, o, tok); err != nil {
		return "", "", err // a concurrent create is redone on the requeue
	}
	return hash, "", nil
}

// ownedSecret reads o's token Secret through r: nil if there is none, and
// a *notOwnedError if one of that name is not o's, or holds no token.
func ownedSecret(ctx context.Context, r client.Reader, o tokenOwner) (*corev1.Secret, error) {
	var s corev1.Secret
	err := r.Get(ctx, client.ObjectKey{Namespace: o.obj.GetNamespace(), Name: o.secret}, &s)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	case len(s.Data[tokenKey]) == 0 || !ownedBy(&s, o.obj):
		return nil, &notOwnedError{secret: o.secret, kind: o.kind}
	}
	return &s, nil
}

// notOwnedError is a Secret of a token's name that is not its owner's
// token. It is ErrNotOwned.
type notOwnedError struct{ secret, kind string }

func (e *notOwnedError) problem() string {
	return fmt.Sprintf("Secret %s exists but is not this %s's token", e.secret, e.kind)
}
func (e *notOwnedError) Error() string        { return e.problem() }
func (e *notOwnedError) Is(target error) bool { return target == ErrNotOwned }

// createToken creates o's token Secret holding tok, labelled with o's name
// and owned by o.
func createToken(ctx context.Context, c client.Client, o tokenOwner, tok string) error {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: o.secret, Namespace: o.obj.GetNamespace(), Labels: map[string]string{o.label: o.obj.GetName()}},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{tokenKey: []byte(tok)},
	}
	if err := controllerutil.SetControllerReference(o.obj, s, c.Scheme()); err != nil {
		return err
	}
	return c.Create(ctx, s)
}

// rotateToken mints a new token for o into its Secret, replacing what it
// held, and writes its hash into o's status: what the reconciler does when
// the Secret is deleted, done at once, so it then finds nothing to do. The
// old token stops when the status changes.
//
// The Secret, read fresh, is the fence: it is written first, conditionally
// on the version read, so of two rotations at once one wins and the other
// is a conflict, and a lost status write is redone from it by the
// reconciler. The status write then converges on it: on a conflict o is
// read again, through fresh, into o.obj, and is done if it holds the hash
// already (the reconciler took it from the Secret), or written again, three
// tries at most. Last, the Secret must still hold the token: another
// rotation may have replaced it since. Every lost race is ErrConflict.
func rotateToken(ctx context.Context, c client.Client, fresh client.Reader, o tokenOwner) (string, error) {
	tok, hash, err := token.Mint(o.obj.GetNamespace(), o.obj.GetName())
	if err != nil {
		return "", err
	}
	// What it answers names the Secret only: the API server's own words
	// stay here.
	meanwhile := fmt.Errorf("the token Secret %s changed meanwhile, rotated or edited by someone else: ask them for the token, or rotate it again: %w", o.secret, storage.ErrConflict)
	sec, err := ownedSecret(ctx, fresh, o)
	if err != nil {
		return "", err
	}
	afterSecretRead()
	if sec == nil {
		err = createToken(ctx, c, o, tok)
	} else {
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Data[tokenKey] = []byte(tok) // the other keys stay as they are
		err = c.Update(ctx, sec)
	}
	switch {
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err), apierrors.IsNotFound(err):
		return "", meanwhile
	case err != nil:
		return "", err
	}
	beforeStatusWrite()
	key := client.ObjectKeyFromObject(o.obj)
	for try := 0; ; try++ {
		if o.status.TokenHash == hash {
			break
		}
		setToken(o, hash)
		err := c.Status().Update(ctx, o.obj)
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) || try == 2 {
			if apierrors.IsConflict(err) {
				return "", fmt.Errorf("the %s kept changing while its token was rotated; try again: %w", o.kind, storage.ErrConflict)
			}
			return "", err
		}
		if err := fresh.Get(ctx, key, o.obj); err != nil {
			return "", err
		}
	}
	switch cur, err := ownedSecret(ctx, fresh, o); {
	case err != nil:
		return "", err
	case cur == nil || string(cur.Data[tokenKey]) != tok:
		return "", meanwhile
	}
	return tok, nil
}

// Test hooks: a rotation's Secret read and its status write.
var afterSecretRead, beforeStatusWrite = func() {}, func() {}

// ErrNotOwned means a Secret of a token's name is not its owner's.
var ErrNotOwned = errors.New("not the owner's token")

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
