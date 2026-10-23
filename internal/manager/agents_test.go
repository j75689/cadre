package manager

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

func TestClaimableBy(t *testing.T) {
	runnerAgent := ReadyAgent(ns, "a", "m", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeRunner, Selector: map[string]string{"pool": "claude-code", "zone": "a"}})
	builtinAgent := ReadyAgent(ns, "b", "m", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
	stale := ReadyAgent(ns, "c", "m", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
	stale.Generation = 2 // spec changed since it was found Ready
	notReady := ReadyAgent(ns, "d", "m", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
	notReady.Status.Conditions[0].Status = metav1.ConditionFalse

	for name, tc := range map[string]struct {
		agent  *v1alpha1.Agent
		labels map[string]string
		want   bool
	}{
		"runner, every pair":         {runnerAgent, map[string]string{"pool": "claude-code", "zone": "a", "extra": "x"}, true},
		"runner, one pair missing":   {runnerAgent, map[string]string{"pool": "claude-code"}, false},
		"runner, a value differs":    {runnerAgent, map[string]string{"pool": "claude-code", "zone": "b"}, false},
		"runner, builtin label only": {runnerAgent, builtin, false},
		"builtin by cadre-runner":    {builtinAgent, builtin, true},
		"builtin by another runner":  {builtinAgent, map[string]string{"pool": "claude-code"}, false},
		"ready for an older spec":    {stale, builtin, false},
		"not ready":                  {notReady, builtin, false},
		"missing":                    {nil, builtin, false},
	} {
		if got := claimableBy(tc.agent, tc.labels); got != tc.want {
			t.Errorf("%s: claimable %v, want %v", name, got, tc.want)
		}
	}
}

// A task whose agent is missing or not Ready is passed over, not an error,
// and the claim takes the next one. The claim carries the agent and its
// model.
func TestClaimSkipsUnclaimableAgents(t *testing.T) {
	e := newEnv(t, 3)
	agents := StaticAgents{
		Agents: map[string]*v1alpha1.Agent{
			ns + "/coder": ReadyAgent(ns, "coder", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin}),
			ns + "/broken": func() *v1alpha1.Agent {
				a := ReadyAgent(ns, "broken", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
				a.Status.Conditions[0].Status = metav1.ConditionFalse
				return a
			}(),
		},
		ModelProviders: map[string]*v1alpha1.ModelProvider{
			ns + "/claude": {Spec: v1alpha1.ModelProviderSpec{Type: v1alpha1.ProviderAnthropic, Model: "claude-opus-5-5"}},
		},
	}
	m := New(e.store, Config{Agents: agents}, WithClock(e.clock.now))
	for _, tk := range []struct{ name, agent string }{{"a-ghost", "ghost"}, {"b-broken", "broken"}, {"c-ok", "coder"}} {
		key := e.addTask(t, tk.name, v1alpha1.AssigneeAgent)
		if tk.agent != "coder" {
			e.reassign(t, key, tk.agent)
		}
	}
	c, err := m.Claim(ctx, ClaimRequest{Runner: "r", Labels: builtin})
	if err != nil || c == nil || c.Task.Name != "c-ok" {
		t.Fatalf("claim: %+v, %v; want c-ok, the only task with a claimable agent", c, err)
	}
	if c.Agent == nil || c.Agent.Name != "coder" || c.ModelProvider == nil || c.ModelProvider.Spec.Model != "claude-opus-5-5" {
		t.Fatalf("claimed agent %+v, model %+v", c.Agent, c.ModelProvider)
	}
	if e.running(t) != 1 {
		t.Fatalf("running %d; the passed-over tasks must not hold slots", e.running(t))
	}
	if c, err := m.Claim(ctx, ClaimRequest{Runner: "r", Labels: map[string]string{"pool": "x"}}); c != nil || err != nil {
		t.Fatalf("claim by a runner no agent matches: %+v, %v", c, err)
	}
}
