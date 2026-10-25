package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/token"
)

// What Runners and AgentAccesses share: one ServiceAccount is held by one
// of each kind in a namespace, decided alike by every reader, and a token
// minted for one is checked alike.

// sharing maps an object naming a ServiceAccount, a Runner or an
// AgentAccess, to the others of its kind in its namespace naming the same
// one: saOf is its index function, and list lists them.
func sharing[T any, P interface {
	*T
	client.Object
}](c client.Reader, saOf func(client.Object) []string, list func(context.Context, client.Reader, string, string) ([]T, error)) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		sa := saOf(o)
		if len(sa) == 0 {
			return nil
		}
		others, err := list(ctx, c, o.GetNamespace(), sa[0])
		if err != nil {
			return nil
		}
		var reqs []reconcile.Request
		for i := range others {
			if other := P(&others[i]); other.GetName() != o.GetName() {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(other)})
			}
		}
		return reqs
	}
}

// inUse is the message of a Runner or an AgentAccess (kind), and of an
// Agent on a Runner, whose ServiceAccount another of its kind holds.
func inUse(kind, sa, holder string) string {
	return fmt.Sprintf("ServiceAccount %s is %s %s's, the oldest %s naming it (then by name); give this %s a ServiceAccount of its own", sa, kind, holder, kind, kind)
}

// holder is the one of objs, the Runners or the AgentAccesses naming a
// ServiceAccount, that holds it: the oldest, then by name, so that every
// reader picks the same one.
func holder[T any, P interface {
	*T
	client.Object
}](objs []T) P {
	if len(objs) == 0 {
		return nil
	}
	h := slices.MinFunc(objs, func(a, b T) int {
		pa, pb := P(&a), P(&b)
		return cmp.Or(pa.GetCreationTimestamp().Compare(pb.GetCreationTimestamp().Time), strings.Compare(pa.GetName(), pb.GetName()))
	})
	return &h
}

// saPrefix starts a ServiceAccount's username.
const saPrefix = "system:serviceaccount:"

// serviceAccountOf reads a TokenReview's username,
// system:serviceaccount:<namespace>:<name>; ok is false for any other.
func serviceAccountOf(username string) (ns, name string, ok bool) {
	rest, ok := strings.CutPrefix(username, saPrefix)
	if !ok {
		return "", "", false
	}
	return strings.Cut(rest, ":")
}

// checkMinted reads the object ns/name a minted token names through r and
// returns it if tok is its: it names no ServiceAccount, and holds tok's
// hash, as tokenOf says them. Every refusal is runner.ErrUnauthenticated,
// so a caller learns nothing about which part was wrong; any other error
// is a read that failed.
func checkMinted[T any, P interface {
	*T
	client.Object
}](ctx context.Context, r client.Reader, ns, name, tok string, tokenOf func(P) (*v1alpha1.ServiceAccountRef, *v1alpha1.MintedToken)) (P, error) {
	obj := P(new(T))
	err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, obj)
	switch {
	case apierrors.IsNotFound(err):
		return nil, runner.ErrUnauthenticated
	case err != nil:
		return nil, fmt.Errorf("read %T: %w", obj, err)
	}
	if sa, st := tokenOf(obj); sa != nil || !token.Matches(tok, st.TokenHash) {
		return nil, runner.ErrUnauthenticated
	}
	return obj, nil
}
