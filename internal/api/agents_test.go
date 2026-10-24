package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage/memory"
)

func TestListAgents(t *testing.T) {
	ready := manager.ReadyAgent(ns, "writer", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
	broken := manager.ReadyAgent(ns, "coder", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
	broken.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, ObservedGeneration: 1, Reason: "ModelProviderNotFound", Message: "ModelProvider claude does not exist"}}
	edited := manager.ReadyAgent(ns, "reviewer", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
	edited.Generation = 2 // Ready was decided for the spec before
	other := manager.ReadyAgent("team-b", "elsewhere", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
	agents := manager.StaticAgents{Agents: map[string]*v1alpha1.Agent{}}
	for _, a := range []*v1alpha1.Agent{ready, broken, edited, other} {
		agents.Agents[a.Namespace+"/"+a.Name] = a
	}
	m := manager.New(memory.NewBackend().Open(), manager.Config{Agents: agents})
	mux := http.NewServeMux()
	api.NewServer(m, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	list := func(namespace string) (int, api.AgentList) {
		t.Helper()
		res, err := http.Get(srv.URL + api.Base + "/namespaces/" + namespace + "/agents")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var out api.AgentList
		if res.StatusCode == http.StatusOK {
			if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
		}
		return res.StatusCode, out
	}

	if code, _ := list(ns); code != http.StatusNotFound {
		t.Fatalf("no Team: %d, want 404", code)
	}
	c := client.New(srv.URL, nil)
	for _, n := range []string{ns, "team-c"} {
		if _, err := c.InitTeam(ctx, n, api.TeamRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if code, got := list("team-c"); code != http.StatusOK || got.Items == nil || len(got.Items) != 0 {
		t.Fatalf("no Agents: %d %+v, want an empty list", code, got)
	}
	_, got := list(ns)
	want := []api.AgentSummary{
		{Name: "coder", Reason: "ModelProviderNotFound", Message: "ModelProvider claude does not exist"},
		{Name: "reviewer", Reason: "Pending", Message: "its current spec has not been checked yet"},
		{Name: "writer", Ready: true},
	}
	if len(got.Items) != len(want) {
		t.Fatalf("agents %+v, want %+v", got.Items, want)
	}
	for i := range want {
		if got.Items[i] != want[i] {
			t.Errorf("agent %d: %+v, want %+v", i, got.Items[i], want[i])
		}
	}
}
