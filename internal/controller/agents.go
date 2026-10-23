package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime/pkg/manager"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/mcp"
	"github.com/j75689/cadre/internal/storage"
)

// Agents gives the Task Manager the controller manager's view of Agents:
// cached reads for pre-filtering claim candidates, and direct reads from
// the API server for the claim decision.
func Agents(mgr ctrl.Manager) manager.Agents { return sources(mgr) }

// ToolServerSources gives the per-run MCP endpoint cached reads of Agents
// and ToolServers for listing tools, and fresh reads of ToolServers and
// their Secrets for making calls.
func ToolServerSources(mgr ctrl.Manager) mcp.Sources { return sources(mgr) }

func sources(mgr ctrl.Manager) agents {
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

func (a agents) CachedToolServer(ctx context.Context, ns, name string) (*v1alpha1.ToolServer, error) {
	return get[v1alpha1.ToolServer](ctx, a.cached, ns, name)
}

func (a agents) ToolServer(ctx context.Context, ns, name string) (*v1alpha1.ToolServer, error) {
	return get[v1alpha1.ToolServer](ctx, a.fresh, ns, name)
}

// SecretValue reads one Secret key directly from the API server. The value
// is returned to the caller only, never cached or logged.
func (a agents) SecretValue(ctx context.Context, ns string, ref v1alpha1.SecretKeyRef) (string, error) {
	s, err := get[corev1.Secret](ctx, a.fresh, ns, ref.Name)
	if err != nil {
		return "", err
	}
	v, ok := s.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("secret %s/%s has no key %s: %w", ns, ref.Name, ref.Key, storage.ErrNotFound)
	}
	return string(v), nil
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
