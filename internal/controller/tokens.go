package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// resource is the owner's API resource, for errors.
func (o tokenOwner) resource() string {
	k := strings.ToLower(o.kind)
	if strings.HasSuffix(k, "s") {
		return k + "es"
	}
	return k + "s"
}

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

// A rotator mints a new token for a token owner into its Secret, replacing
// what it held, and writes its hash into the owner's status: what the
// reconciler does when the Secret is deleted, done at once, so it then
// finds nothing to do. The old token stops when the status changes.
//
// The Secret, read fresh, is the fence: it is written first, conditionally
// on the version read, so of two rotations at once one wins and the other
// is a conflict, and a lost status write is redone from it by the
// reconciler. The status write then converges on it, retried with a short
// backoff for statusWait: on a conflict the owner and its Secret are read
// again, fresh; if the Secret no longer holds the token another rotation
// won, and this one is a conflict; if the status holds the hash already
// (the reconciler took it from the Secret) it is done. Last, the Secret
// must still hold the token. Every lost race is ErrConflict.
type rotator struct {
	c     client.Client
	fresh client.Reader
	// Test hooks, after the Secret is read and before the status is
	// written; nil in production.
	afterSecretRead, beforeStatusWrite func()
}

// statusWait bounds how long a rotation keeps writing a status others
// keep changing.
const statusWait = 2 * time.Second

// opError is an API server's answer to verb on resource, for the caller to
// tell a token's Secret from its owner.
type opError struct {
	resource, verb string
	err            error
}

func (e *opError) Error() string { return e.verb + " " + e.resource + ": " + e.err.Error() }
func (e *opError) Unwrap() error { return e.err }

func (r rotator) rotate(ctx context.Context, o tokenOwner) (string, error) {
	tok, hash, err := token.Mint(o.obj.GetNamespace(), o.obj.GetName())
	if err != nil {
		return "", err
	}
	// What it answers names the Secret only: the API server's own words
	// stay here.
	meanwhile := fmt.Errorf("the token Secret %s changed meanwhile, rotated or edited by someone else: ask them for the token, or rotate it again: %w", o.secret, storage.ErrConflict)
	holds := func() (bool, error) {
		cur, err := ownedSecret(ctx, r.fresh, o)
		if err != nil {
			return false, wrapOp("secrets", "get", err)
		}
		return cur != nil && string(cur.Data[tokenKey]) == tok, nil
	}
	sec, err := ownedSecret(ctx, r.fresh, o)
	if err != nil {
		return "", wrapOp("secrets", "get", err)
	}
	if r.afterSecretRead != nil {
		r.afterSecretRead()
	}
	verb := "update"
	if sec == nil {
		verb = "create"
		err = createToken(ctx, r.c, o, tok)
	} else {
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Data[tokenKey] = []byte(tok) // the other keys stay as they are
		err = r.c.Update(ctx, sec)
	}
	switch {
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err), apierrors.IsNotFound(err):
		return "", meanwhile
	case err != nil:
		return "", &opError{"secrets", verb, err}
	}
	if r.beforeStatusWrite != nil {
		r.beforeStatusWrite()
	}
	key := client.ObjectKeyFromObject(o.obj)
	deadline := time.Now().Add(statusWait)
	for pause := 20 * time.Millisecond; o.status.TokenHash != hash; pause = min(2*pause, 200*time.Millisecond) {
		setToken(o, hash)
		err := r.c.Status().Update(ctx, o.obj)
		switch {
		case err == nil:
			continue // the status has the hash: the loop ends
		case !apierrors.IsConflict(err):
			return "", &opError{o.resource(), "update the status of", err}
		case time.Now().After(deadline):
			return "", fmt.Errorf("the new token is in the Secret %s, but the %s kept changing while its hash was written: the old token stops within a minute, when the reconciler takes the new one's hash from the Secret: %w", o.secret, o.kind, storage.ErrConflict)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pause):
		}
		if err := r.fresh.Get(ctx, key, o.obj); err != nil {
			return "", &opError{o.resource(), "get", err}
		}
		switch ok, err := holds(); {
		case err != nil:
			return "", err
		case !ok:
			return "", meanwhile // another rotation won: its hash stands
		}
	}
	switch ok, err := holds(); {
	case err != nil:
		return "", err
	case !ok:
		return "", meanwhile
	}
	return tok, nil
}

// wrapOp is err, from verb on resource, as an *opError, unless it is not
// the API server's answer.
func wrapOp(resource, verb string, err error) error {
	var notOwned *notOwnedError
	if errors.As(err, &notOwned) {
		return err
	}
	return &opError{resource, verb, err}
}

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
