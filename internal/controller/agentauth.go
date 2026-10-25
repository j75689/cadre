package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/token"
)

// freshReadTimeout bounds the read of an AgentAccess for a minted token.
const freshReadTimeout = 5 * time.Second

// AgentAccesses authenticates agents outside Cadre on the board. A minted
// token (two parts) is checked against the AgentAccess it names, read
// fresh through fresh (a controller manager's API reader) on every
// request: it must still be one without a ServiceAccount, and hold the
// token's hash. Its refusals are remembered for a moment, by the token's
// hash, its acceptances never; an API error is no refusal. A
// ServiceAccount token (a JWT) goes to reviews, which may be cached, and
// is the AgentAccess holding the ServiceAccount, found through the index
// in c (a controller manager's client) and decided from the specs as the
// reconciler decides it. Neither looks at Ready. A token is never taken
// for a Runner's, whose minted tokens have the same shape.
func AgentAccesses(c client.Client, fresh client.Reader, reviews runner.Authenticator, log *slog.Logger) auth.Agents {
	return agentAccesses{
		c: c, reviews: reviews, log: log,
		minted: runner.Refusals(mintedAccess{c: fresh}),
	}
}

type agentAccesses struct {
	c       client.Client
	reviews runner.Authenticator
	minted  runner.Authenticator
	log     *slog.Logger
}

func (a agentAccesses) Authenticate(ctx context.Context, tok string) (*auth.Identity, error) {
	if strings.Count(tok, ".") == 1 {
		id, err := a.minted.Authenticate(ctx, tok)
		if err != nil {
			return nil, unauthenticated(err)
		}
		ns, name, _ := strings.Cut(id.Name, "/")
		return auth.AgentIdentity(ns, name, id.Scope[ns]["role"]), nil
	}
	id, err := a.reviews.Authenticate(ctx, tok)
	if err != nil {
		return nil, unauthenticated(err)
	}
	ns, sa, ok := strings.Cut(strings.TrimPrefix(id.Name, saPrefix), ":")
	if !ok || !strings.HasPrefix(id.Name, saPrefix) {
		return nil, auth.ErrUnauthenticated
	}
	list, err := accessSharers(ctx, a.c, ns, sa)
	if err != nil {
		return nil, err
	}
	h := holder(list)
	if h == nil {
		// A Runner's ServiceAccount, cadre-runner's, or one nobody gave
		// board access: the name only, never the token.
		a.log.Debug("a ServiceAccount no AgentAccess names was refused on the board", "serviceAccount", id.Name)
		return nil, auth.ErrUnauthenticated
	}
	return auth.AgentIdentity(h.Namespace, h.Name, string(h.Spec.Role)), nil
}

// unauthenticated is err as auth tells it: a refusal, or not.
func unauthenticated(err error) error {
	if errors.Is(err, runner.ErrUnauthenticated) {
		return auth.ErrUnauthenticated
	}
	return err
}

// mintedAccess checks a minted token against its AgentAccess, read through
// c. Every refusal is runner.ErrUnauthenticated, so a caller learns nothing
// about which part was wrong, and runner.Refusals remembers it; the
// identity it returns carries the AgentAccess as namespace/name and its
// role under Scope.
type mintedAccess struct{ c client.Reader }

func (m mintedAccess) Authenticate(ctx context.Context, tok string) (runner.Identity, error) {
	ns, name, ok := token.Locate(tok)
	if !ok {
		return runner.Identity{}, runner.ErrUnauthenticated
	}
	ctx, cancel := context.WithTimeout(ctx, freshReadTimeout)
	defer cancel()
	var aa v1alpha1.AgentAccess
	err := m.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &aa)
	switch {
	case apierrors.IsNotFound(err):
		return runner.Identity{}, runner.ErrUnauthenticated
	case err != nil:
		return runner.Identity{}, fmt.Errorf("read agent access: %w", err)
	}
	if aa.Spec.ServiceAccount != nil || !token.Matches(tok, aa.Status.TokenHash) {
		return runner.Identity{}, runner.ErrUnauthenticated
	}
	return runner.Identity{Name: ns + "/" + name, Scope: map[string]map[string]string{ns: {"role": string(aa.Spec.Role)}}}, nil
}
