package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/storage"
)

// +kubebuilder:rbac:groups=cadre.dev,resources=agentaccesses,verbs=create;update;delete

// AgentAccessStore keeps AgentAccesses for the API through c, a controller
// manager's client: a list is read from its cache, one AgentAccess fresh
// through fresh (the manager's API reader), since a PUT decides from it
// whether it makes or edits, and writes go to the API server. Its answers
// map to the store's errors, so the API answers them as it does the
// board's.
func AgentAccessStore(c client.Client, fresh client.Reader) api.AgentAccessStore {
	return accessStore{c: c, fresh: fresh}
}

type accessStore struct {
	c     client.Client
	fresh client.Reader
}

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

// storeError is the API server's answer to verb on AgentAccess name as the
// store's errors say it. A refusal of the object itself keeps the API
// server's reason, which says what the CRD wants; one of cadre-server is
// api.ErrNotPermitted, naming the verb.
func storeError(verb, name string, err error) error {
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
		return fmt.Errorf("%s agentaccesses: %w: %w", verb, api.ErrNotPermitted, err)
	}
	return fmt.Errorf("%s agent access %s: %w", verb, name, err)
}
