package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage/memory"
)

// A kind's list, which the next kinds are served by, says what GET /agents
// says of the same Agents.
func TestListConfigIsTheAgentList(t *testing.T) {
	ready := manager.ReadyAgent("team-a", "writer", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
	edited := manager.ReadyAgent("team-a", "reviewer", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
	edited.Generation = 2
	broken := manager.ReadyAgent("team-a", "coder", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
	broken.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, ObservedGeneration: 1, Reason: "ModelProviderNotFound", Message: "ModelProvider claude does not exist"}}
	agents := manager.StaticAgents{Agents: map[string]*v1alpha1.Agent{}}
	for _, a := range []*v1alpha1.Agent{ready, edited, broken} {
		agents.Agents[a.Namespace+"/"+a.Name] = a.DeepCopy()
	}
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store := memory.NewBackend().Open()
	if err := store.CreateTeam(context.Background(), &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: v1alpha1.TeamName}}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(manager.New(store, manager.Config{Agents: agents}), slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithConfig(fake.NewClientBuilder().WithScheme(scheme).WithObjects(ready, edited, broken).Build()))
	get := func(h func(w *httptest.ResponseRecorder)) string {
		w := httptest.NewRecorder()
		h(w)
		return w.Body.String()
	}
	req := httptest.NewRequest("GET", Base+"/namespaces/team-a/agents", nil)
	req.SetPathValue("ns", "team-a")
	want := get(func(w *httptest.ResponseRecorder) { s.listAgents(w, req) })
	if !strings.Contains(want, `"coder"`) {
		t.Fatalf("GET /agents %s", want)
	}
	if got := get(func(w *httptest.ResponseRecorder) { s.listConfig(configKinds[0])(w, req) }); got != want {
		t.Errorf("listConfig %s\nGET /agents %s", got, want)
	}
}
