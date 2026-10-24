package manager

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
)

// Agents gives the Task Manager the configuration a claim needs. Agents and
// ModelProviders are CRDs, not storage-driver data, so they come from here.
// All three methods return storage.ErrNotFound for a missing object.
type Agents interface {
	// CachedAgent may answer from a cache. It only pre-filters claim
	// candidates; no decision rests on it.
	CachedAgent(ctx context.Context, namespace, name string) (*v1alpha1.Agent, error)
	// Agent reads the Agent fresh, for the claim decision.
	Agent(ctx context.Context, namespace, name string) (*v1alpha1.Agent, error)
	// ModelProvider reads the ModelProvider fresh.
	ModelProvider(ctx context.Context, namespace, name string) (*v1alpha1.ModelProvider, error)
}

// claimableBy reports whether a runner sending labels may claim the tasks of
// agent: the agent is Ready for its current spec, and its runtime matches.
// A builtin agent is claimed only by cadre-runner, which sends
// cadre.dev/runtime: builtin; a runner agent by any runner whose labels
// include every pair of its selector.
func claimableBy(agent *v1alpha1.Agent, labels map[string]string) bool {
	if agent == nil || !agent.Status.ReadyFor(agent.Generation) {
		return false
	}
	switch {
	case agent.Spec.Runtime.Builtin():
		return labels[v1alpha1.LabelRuntime] == string(v1alpha1.RuntimeBuiltin)
	case agent.Spec.Runtime.Type == v1alpha1.RuntimeRunner:
		if len(agent.Spec.Runtime.Selector) == 0 {
			return false
		}
		for k, v := range agent.Spec.Runtime.Selector {
			if got, ok := labels[k]; !ok || got != v {
				return false
			}
		}
		return true
	}
	return false
}

// StaticAgents serves fixed Agents and ModelProviders, for tests and for
// drivers without a cluster. Both lookups read the same map.
type StaticAgents struct {
	Agents         map[string]*v1alpha1.Agent         // by "namespace/name"
	ModelProviders map[string]*v1alpha1.ModelProvider // by "namespace/name"
}

// CachedAgent implements Agents.
func (s StaticAgents) CachedAgent(ctx context.Context, ns, name string) (*v1alpha1.Agent, error) {
	return s.Agent(ctx, ns, name)
}

// Agent implements Agents.
func (s StaticAgents) Agent(_ context.Context, ns, name string) (*v1alpha1.Agent, error) {
	if a, ok := s.Agents[ns+"/"+name]; ok {
		return a.DeepCopy(), nil
	}
	return nil, fmt.Errorf("agent %s/%s: %w", ns, name, storage.ErrNotFound)
}

// ModelProvider implements Agents.
func (s StaticAgents) ModelProvider(_ context.Context, ns, name string) (*v1alpha1.ModelProvider, error) {
	if mp, ok := s.ModelProviders[ns+"/"+name]; ok {
		return mp.DeepCopy(), nil
	}
	return nil, fmt.Errorf("model provider %s/%s: %w", ns, name, storage.ErrNotFound)
}

// ReadyAgent returns an Agent that reports Ready for its spec, with the
// given runtime; for tests and for drivers without controllers.
func ReadyAgent(namespace, name, modelRef string, runtime v1alpha1.AgentRuntime) *v1alpha1.Agent {
	a := &v1alpha1.Agent{Spec: v1alpha1.AgentSpec{ModelRef: modelRef, Runtime: runtime}}
	a.Namespace, a.Name, a.Generation = namespace, name, 1
	a.Status.ObservedGeneration = 1
	a.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "Resolved"}}
	return a
}

// ReadyBuiltin returns Agents holding one Ready builtin agent and the
// ModelProvider it names, as a cluster with working controllers would; for
// tests.
func ReadyBuiltin(namespace, agent, model string) StaticAgents {
	mp := &v1alpha1.ModelProvider{Spec: v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderAnthropic, Model: "claude-opus-5-5"}}
	mp.Namespace, mp.Name = namespace, model
	return StaticAgents{
		Agents:         map[string]*v1alpha1.Agent{namespace + "/" + agent: ReadyAgent(namespace, agent, model, v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})},
		ModelProviders: map[string]*v1alpha1.ModelProvider{namespace + "/" + model: mp},
	}
}
