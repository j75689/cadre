package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/token"
)

// +kubebuilder:rbac:groups=cadre.dev,resources=agentaccesses,verbs=create;update;delete

// AgentAccessStore keeps AgentAccesses for the API through c, a controller
// manager's client: a list is read from its cache, one AgentAccess fresh
// through fresh (the manager's API reader), since a PUT decides from it
// whether it makes or edits, and writes go to the API server. Its answers
// map to the store's errors, so the API answers them as it does the
// board's.
// A rotated token is told with a Normal Event on its AgentAccess, through
// events, naming who rotated it.
func AgentAccessStore(c client.Client, fresh client.Reader, events events.EventRecorder) api.AgentAccessStore {
	return accessStore{c: c, fresh: fresh, events: events, rot: rotator{c: c, fresh: fresh, stillMinted: func(o client.Object) bool {
		return o.(*v1alpha1.AgentAccess).Spec.ServiceAccount == nil
	}}}
}

type accessStore struct {
	c      client.Client
	fresh  client.Reader
	events events.EventRecorder
	rot    rotator
}

// cacheWait is how long a rotation waits for the cache to see its hash:
// the refusal memo's time, so a token shown is refused, if at all, no
// longer than a made-up one is.
const cacheWait = 5 * time.Second

func (s accessStore) ListAgentAccesses(ctx context.Context, ns string) ([]v1alpha1.AgentAccess, error) {
	var list v1alpha1.AgentAccessList
	if err := s.c.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, storeError("list", "", err)
	}
	slices.SortFunc(list.Items, func(a, b v1alpha1.AgentAccess) int { return strings.Compare(a.Name, b.Name) })
	return list.Items, nil
}

func (s accessStore) GetAgentAccess(ctx context.Context, ns, name string) (*v1alpha1.AgentAccess, error) {
	var aa v1alpha1.AgentAccess
	if err := s.fresh.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &aa); err != nil {
		return nil, storeError("get", name, err)
	}
	return &aa, nil
}

func (s accessStore) CreateAgentAccess(ctx context.Context, aa *v1alpha1.AgentAccess) error {
	err := s.c.Create(ctx, aa)
	if apierrors.IsNotFound(err) {
		// The namespace is not there: a request to fix, not a 404.
		return fmt.Errorf("%s: %w", err.Error(), storage.ErrInvalid)
	}
	return storeError("create", aa.Name, err)
}

func (s accessStore) UpdateAgentAccess(ctx context.Context, aa *v1alpha1.AgentAccess) error {
	return storeError("update", aa.Name, s.c.Update(ctx, aa))
}

func (s accessStore) DeleteAgentAccess(ctx context.Context, ns, name string) error {
	return storeError("delete", name, s.c.Delete(ctx, &v1alpha1.AgentAccess{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}))
}

// RotateAgentAccessToken rotates the token, then waits, cacheWait at most,
// for the cache to see the new hash: a minted token is checked against the
// cache first, so one answered before the cache has it would be refused,
// and that refusal remembered, by an agent using it at once. A cache that
// takes longer still gets the answer, the token being good, and is logged.
func (s accessStore) RotateAgentAccessToken(ctx context.Context, ns, name, by string) (string, error) {
	// The reconciler's preconditions, before anything is written: it mints
	// for a DNS-label name in a Team's namespace only.
	if msg := invalidName("AgentAccess", name); msg != "" {
		return "", fmt.Errorf("%s: %w", msg, storage.ErrInvalid)
	}
	// Fresh, as the AgentAccess is: a Team made a moment ago counts.
	switch ok, err := hasTeam(ctx, s.fresh, ns); {
	case err != nil:
		return "", storeErrorOn("teams", "get", v1alpha1.TeamName, err)
	case !ok:
		return "", fmt.Errorf("namespace %s has no Team, and no token is minted outside one: %w", ns, storage.ErrInvalid)
	}
	aa, err := s.GetAgentAccess(ctx, ns, name)
	if err != nil {
		return "", err
	}
	if aa.Spec.ServiceAccount != nil {
		return "", fmt.Errorf("AgentAccess %s authenticates as ServiceAccount %s: it has no minted token: %w", name, aa.Spec.ServiceAccount.Name, storage.ErrInvalid)
	}
	tok, err := s.rot.rotate(ctx, accessTokenOwner(aa))
	var op *opError
	var written *writtenError
	switch {
	case errors.As(err, &written):
		// It took effect: told as a rotation; why its hash is not written
		// is logged here, the API server's words not answered.
		log.FromContext(ctx).Error(written.Err, "a rotated token's hash could not be written", "namespace", ns, "agentAccess", name)
		s.events.Eventf(aa, nil, corev1.EventTypeNormal, "TokenRotated", "RotateToken", "token rotated by %s; its hash is written on the reconciler's next pass", by)
		return "", err
	case errors.Is(err, ErrNotOwned):
		return "", fmt.Errorf("%w; delete it to have a token minted: %w", err, storage.ErrInvalid)
	case errors.Is(err, storage.ErrConflict):
		return "", err
	case errors.As(err, &op):
		return "", storeErrorOn(op.resource, op.verb, name, op.err)
	case err != nil:
		return "", err
	}
	s.events.Eventf(aa, nil, corev1.EventTypeNormal, "TokenRotated", "RotateToken", "token rotated by %s", by)
	hash := token.Hash(tok)
	seen := func(ctx context.Context) (bool, error) {
		var cached v1alpha1.AgentAccess
		err := s.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &cached)
		return err == nil && cached.Status.TokenHash == hash, nil
	}
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, cacheWait, true, seen); err != nil {
		log.FromContext(ctx).Info("the cache had not seen a rotated token's hash in time; answering anyway", "namespace", ns, "agentAccess", name, "waited", cacheWait)
	}
	return tok, nil
}

// storeError is the API server's answer to verb on AgentAccess name as the
// store's errors say it. A refusal of the object itself keeps the API
// server's reason, which says what the CRD wants; one of cadre-server is
// api.ErrNotPermitted, naming the verb.
func storeError(verb, name string, err error) error {
	return storeErrorOn("agentaccesses", verb, name, err)
}

// storeErrorOn is storeError for verb on resource: a token's Secret, say,
// for which a refusal of cadre-server names the Secrets' verb it lacks.
func storeErrorOn(resource, verb, name string, err error) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return fmt.Errorf("agent access %s: %w", name, storage.ErrNotFound)
	case apierrors.IsAlreadyExists(err):
		return fmt.Errorf("agent access %s: %w", name, storage.ErrAlreadyExists)
	case apierrors.IsConflict(err):
		return fmt.Errorf("agent access %s: %w", name, storage.ErrConflict)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return fmt.Errorf("%s: %w", err.Error(), storage.ErrInvalid)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return fmt.Errorf("%s %s: %w: %w", verb, resource, api.ErrNotPermitted, err)
	}
	return fmt.Errorf("%s agent access %s: %w", verb, name, err)
}
