package api

import (
	"context"
	"fmt"

	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

// configEventKinds are the kinds a watch stream hints a change of: the
// configuration kinds and the Team. AgentAccess is not one: only those who
// manage access may list it, and a hint would tell a viewer its name.
var configEventKinds = []struct {
	kind string
	obj  client.Object
}{
	{"Agent", &v1alpha1.Agent{}},
	{"ModelProvider", &v1alpha1.ModelProvider{}},
	{"ToolServer", &v1alpha1.ToolServer{}},
	{"Schedule", &v1alpha1.Schedule{}},
	{"Notifier", &v1alpha1.Notifier{}},
	{"Runner", &v1alpha1.Runner{}},
	{"Team", &v1alpha1.Team{}},
}

// ConfigEvents feeds h a config hint whenever a configuration resource or a
// Team changes in c, and its streams then say so. Call it before serving.
func ConfigEvents(ctx context.Context, c cache.Informers, h *Hub) error {
	for _, k := range configEventKinds {
		inf, err := c.GetInformer(ctx, k.obj)
		if err != nil {
			return fmt.Errorf("informer for %T: %w", k.obj, err)
		}
		if _, err := inf.AddEventHandler(h.configHandler(k.kind)); err != nil {
			return fmt.Errorf("watch %T: %w", k.obj, err)
		}
	}
	h.config = true
	return nil
}

// configHandler hints the changes to kind's resources. What the informer
// had already is no change: the resources it lists as the handler is added,
// and the same version of one again at a resync or a relist. Every write the
// API server keeps, to the spec, the status or the metadata, gives a new
// resourceVersion.
func (h *Hub) configHandler(kind string) toolscache.ResourceEventHandler {
	return toolscache.ResourceEventHandlerDetailedFuncs{
		AddFunc: func(obj any, initial bool) {
			if !initial {
				h.publishConfig(kind, obj.(client.Object))
			}
		},
		UpdateFunc: func(old, obj any) {
			if o := obj.(client.Object); o.GetResourceVersion() != old.(client.Object).GetResourceVersion() {
				h.publishConfig(kind, o)
			}
		},
		DeleteFunc: func(obj any) {
			// A delete the informer missed comes as its last known state.
			if gone, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
				obj = gone.Obj
			}
			h.publishConfig(kind, obj.(client.Object))
		},
	}
}

// publishConfig hints o's namespace's streams that o changed. It never
// blocks, so the informer that calls it never waits.
func (h *Hub) publishConfig(kind string, o client.Object) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.send(o.GetNamespace(), watchEvent{kind: "config", key: "config/" + kind + "/" + o.GetName(), data: ConfigHint{Kind: kind, Name: o.GetName()}})
}
