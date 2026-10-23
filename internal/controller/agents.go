package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime/pkg/manager"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
)

// Agents gives the Task Manager the controller manager's view of Agents:
// cached reads for pre-filtering claim candidates, and direct reads from
// the API server for the claim decision.
func Agents(mgr ctrl.Manager) manager.Agents {
	return agents{cached: mgr.GetClient(), fresh: mgr.GetAPIReader()}
}

type agents struct {
	cached client.Reader
	fresh  client.Reader
}

func (a agents) CachedAgent(ctx context.Context, ns, name string) (*v1alpha1.Agent, error) {
	return get[v1alpha1.Agent](ctx, a.cached, ns, name)
}

func (a agents) Agent(ctx context.Context, ns, name string) (*v1alpha1.Agent, error) {
	return get[v1alpha1.Agent](ctx, a.fresh, ns, name)
}

func (a agents) ModelProvider(ctx context.Context, ns, name string) (*v1alpha1.ModelProvider, error) {
	return get[v1alpha1.ModelProvider](ctx, a.fresh, ns, name)
}

// get reads one object, mapping NotFound to storage.ErrNotFound.
func get[T any, PT interface {
	*T
	client.Object
}](ctx context.Context, r client.Reader, ns, name string) (*T, error) {
	obj := PT(new(T))
	err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, obj)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%T %s/%s: %w", obj, ns, name, storage.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return (*T)(obj), nil
}
