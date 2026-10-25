package controller

import (
	"context"
	"sync"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// jobVerbs are what cadre-server does to the Jobs of an Agent's runs.
var jobVerbs = []string{"create", "delete", "patch"}

// jobAccess answers whether cadre-server may run Jobs in a namespace: may
// create, delete and patch them there. The answer depends on the namespace
// only, so it is kept for one resync and shared by every Agent there,
// rather than asked again with three SelfSubjectAccessReviews per Agent
// per reconcile. A binding added or removed shows up once it expires.
type jobAccess struct {
	client client.Client
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	answers map[string]jobAnswer // by namespace
}

type jobAnswer struct {
	allowed bool
	until   time.Time
}

func newJobAccess(c client.Client, ttl time.Duration) *jobAccess {
	return &jobAccess{client: c, ttl: ttl, now: time.Now, answers: map[string]jobAnswer{}}
}

// allowed reports whether cadre-server may run Jobs in ns, from the answer
// kept for ns while it lasts.
func (a *jobAccess) allowed(ctx context.Context, ns string) (bool, error) {
	now := a.now()
	a.mu.Lock()
	ans, ok := a.answers[ns]
	a.mu.Unlock()
	if ok && now.Before(ans.until) {
		return ans.allowed, nil
	}
	allowed, err := a.review(ctx, ns)
	if err != nil {
		return false, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Forget namespaces not asked about for a while, deleted ones say.
	for k, v := range a.answers {
		if !now.Before(v.until) {
			delete(a.answers, k)
		}
	}
	a.answers[ns] = jobAnswer{allowed: allowed, until: now.Add(a.ttl)}
	return allowed, nil
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
		if !review.Status.Allowed {
			return false, nil
		}
	}
	return true, nil
}
