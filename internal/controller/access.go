package controller

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// jobVerbs are what cadre-server does to the Jobs of an Agent's runs.
var jobVerbs = []string{"create", "delete", "patch"}

// jobAccess answers whether cadre-server may run Jobs in a namespace: may
// create, delete and patch them there. The answer depends on the namespace
// only, so an allowance is kept for one resync and shared by every Agent
// there, rather than asked again with three SelfSubjectAccessReviews per
// Agent per reconcile; a binding removed shows once it expires. A refusal
// is not kept: it is rare, cheap to ask again, and a binding added must
// show on the Agent's next event.
type jobAccess struct {
	client client.Client
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	allowed map[string]time.Time // namespace: until when
}

func newJobAccess(c client.Client, ttl time.Duration, now func() time.Time) *jobAccess {
	return &jobAccess{client: c, ttl: ttl, now: now, allowed: map[string]time.Time{}}
}

// check reports whether cadre-server may run Jobs in ns. An authorizer
// that could not decide is an error, not a refusal.
func (a *jobAccess) check(ctx context.Context, ns string) (bool, error) {
	now := a.now()
	if _, ok := a.expiry(ns); ok {
		return true, nil
	}
	allowed, err := a.review(ctx, ns)
	a.mu.Lock()
	defer a.mu.Unlock()
	// Forget namespaces not asked about for a while, deleted ones say.
	for k, v := range a.allowed {
		if !now.Before(v) {
			delete(a.allowed, k)
		}
	}
	if err != nil || !allowed {
		return false, err
	}
	a.allowed[ns] = now.Add(a.ttl)
	return true, nil
}

// expiry says until when the allowance kept for ns lasts, if one is: an
// Agent resolved with it checks again then.
func (a *jobAccess) expiry(ns string) (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	until, ok := a.allowed[ns]
	return until, ok && a.now().Before(until)
}

// review asks the API server, one verb at a time, until one is refused.
func (a *jobAccess) review(ctx context.Context, ns string) (bool, error) {
	for _, verb := range jobVerbs {
		review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: ns, Verb: verb, Group: "batch", Resource: "jobs"},
		}}
		if err := a.client.Create(ctx, review); err != nil {
			return false, err
		}
		// An explicit denial is a refusal, error or not; an allowance
		// stands despite an error from another authorizer.
		if review.Status.Denied {
			return false, nil
		}
		if e := review.Status.EvaluationError; e != "" && !review.Status.Allowed {
			return false, fmt.Errorf("reviewing whether cadre-server may %s Jobs in %s: %s", verb, ns, e)
		}
		if !review.Status.Allowed {
			return false, nil
		}
	}
	return true, nil
}

// authzRefusal matches the API server's message for a request an
// authorizer refused: RBAC most often, but a webhook or ABAC refusal reads
// the same.
var authzRefusal = regexp.MustCompile(`User "[^"]*" cannot \S+ resource `)

// forbiddenMessage is a 403's message, followed by hint, which says what to
// bind, when authorization refused the request. A quota or an admission
// denial is a 403 too, which no binding fixes: there the API server's
// message stands alone.
func forbiddenMessage(err error, hint string) string {
	if authzRefusal.MatchString(err.Error()) {
		return err.Error() + "; " + hint
	}
	return err.Error()
}
