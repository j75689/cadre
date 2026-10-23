package manager

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
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
	// A runner no agent matches gets nothing, while a claimable task waits.
	if c, err := m.Claim(ctx, ClaimRequest{Runner: "r", Labels: map[string]string{"pool": "x"}}); c != nil || err != nil {
		t.Fatalf("claim by a runner no agent matches: %+v, %v", c, err)
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
}

// flakyAgents wraps Agents with injected failures and counts fresh reads.
type flakyAgents struct {
	Agents
	cachedErr  error
	modelErr   error
	freshReads atomic.Int32
}

func (f *flakyAgents) CachedAgent(c context.Context, ns, name string) (*v1alpha1.Agent, error) {
	if f.cachedErr != nil {
		return nil, f.cachedErr
	}
	return f.Agents.CachedAgent(c, ns, name)
}

func (f *flakyAgents) Agent(c context.Context, ns, name string) (*v1alpha1.Agent, error) {
	f.freshReads.Add(1)
	return f.Agents.Agent(c, ns, name)
}

func (f *flakyAgents) ModelProvider(c context.Context, ns, name string) (*v1alpha1.ModelProvider, error) {
	if f.modelErr != nil {
		return nil, f.modelErr
	}
	return f.Agents.ModelProvider(c, ns, name)
}

// A cache that cannot answer (still syncing, say) does not turn into a
// silent "no work": the fresh read decides.
func TestCacheErrorsFallThroughToTheFreshRead(t *testing.T) {
	e := newEnv(t, 1)
	e.addTask(t, "t", v1alpha1.AssigneeAgent)
	f := &flakyAgents{Agents: testAgents, cachedErr: errors.New("cache not started")}
	m := New(e.store, Config{Agents: f}, WithClock(e.clock.now))
	if c, err := m.Claim(ctx, ClaimRequest{Runner: "r", Labels: builtin}); err != nil || c == nil {
		t.Fatalf("claim while the cache fails: %+v, %v", c, err)
	}
}

// The model is read before a slot is taken: a transient failure is reported
// and holds no slot; a missing model passes the task over.
func TestTheModelIsReadBeforeTheSlot(t *testing.T) {
	e := newEnv(t, 1)
	e.addTask(t, "t", v1alpha1.AssigneeAgent)
	f := &flakyAgents{Agents: testAgents, modelErr: errors.New("apiserver timeout")}
	m := New(e.store, Config{Agents: f}, WithClock(e.clock.now))
	if c, err := m.Claim(ctx, ClaimRequest{Runner: "r", Labels: builtin}); c != nil || err == nil || e.running(t) != 0 {
		t.Fatalf("claim while the model read fails: %+v, %v, running %d", c, err, e.running(t))
	}
	f.modelErr = fmt.Errorf("gone: %w", storage.ErrNotFound)
	if c, err := m.Claim(ctx, ClaimRequest{Runner: "r", Labels: builtin}); c != nil || err != nil || e.running(t) != 0 {
		t.Fatalf("claim with the model missing: %+v, %v, running %d", c, err, e.running(t))
	}
}

// A team the index shows full costs no API reads.
func TestAFullTeamCostsNoReads(t *testing.T) {
	e := newEnv(t, 1)
	e.addTask(t, "a", v1alpha1.AssigneeAgent)
	e.addTask(t, "b", v1alpha1.AssigneeAgent)
	e.claim(t)
	f := &flakyAgents{Agents: testAgents}
	m := New(e.store, Config{Agents: f}, WithClock(e.clock.now))
	if c, err := m.Claim(ctx, ClaimRequest{Runner: "r", Labels: builtin}); c != nil || err != nil {
		t.Fatalf("claim on a full team: %+v, %v", c, err)
	}
	if n := f.freshReads.Load(); n != 0 {
		t.Fatalf("%d fresh agent reads on a full team, want 0", n)
	}
}
