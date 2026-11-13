package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/authz"
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
		{Name: "coder", Reason: "ModelProviderNotFound", Message: "ModelProvider claude does not exist", ModelRef: "claude"},
		{Name: "reviewer", Reason: "Pending", Message: "its current spec has not been checked yet", ModelRef: "claude"},
		{Name: "writer", Ready: true, ModelRef: "claude"},
	}
	if len(got.Items) != len(want) {
		t.Fatalf("agents %+v, want %+v", got.Items, want)
	}
	for i := range want {
		if !reflect.DeepEqual(got.Items[i], want[i]) {
			t.Errorf("agent %d: %+v, want %+v", i, got.Items[i], want[i])
		}
	}
	res, err := http.Get(srv.URL + api.Base + "/namespaces/" + ns + "/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	const exact = `{"items":[{"name":"coder","ready":false,"reason":"ModelProviderNotFound","message":"ModelProvider claude does not exist","modelRef":"claude"},{"name":"reviewer","ready":false,"reason":"Pending","message":"its current spec has not been checked yet","modelRef":"claude"},{"name":"writer","ready":true,"modelRef":"claude"}]}` + "\n"
	if string(body) != exact {
		t.Errorf("the body: %s, want %s", body, exact)
	}
}

// An Agent's ModelProvider and ToolServers are a member's to see in the
// list, as its spec is on its own page; a viewer gets its health only.
func TestListAgentsRefsByRole(t *testing.T) {
	writer := manager.ReadyAgent(ns, "writer", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
	writer.Spec.ToolRefs = []string{"github", "search"}
	store := memory.NewBackend().Open()
	roles := []v1alpha1.TeamRole{
		{Role: v1alpha1.RoleViewer, Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectUser, Name: "eve@example.test"}}},
		{Role: v1alpha1.RoleMember, Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectUser, Name: "dana@example.test"}}},
	}
	if err := store.CreateTeam(ctx, &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: v1alpha1.TeamName}, Spec: v1alpha1.TeamSpec{Roles: roles}}); err != nil {
		t.Fatal(err)
	}
	m := manager.New(store, manager.Config{Agents: manager.StaticAgents{Agents: map[string]*v1alpha1.Agent{ns + "/writer": writer}}})
	mux := http.NewServeMux()
	api.NewServer(m, slog.New(slog.NewTextHandler(io.Discard, nil)), api.WithAccess(auth.ModeOIDC, &authz.Policy{})).Register(mux)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), &auth.Identity{Username: r.Header.Get("X-User")})))
	}))
	t.Cleanup(srv.Close)
	list := func(user string) string {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+api.Base+"/namespaces/"+ns+"/agents", nil)
		req.Header.Set("X-User", user)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s lists agents: %d %s", user, res.StatusCode, body)
		}
		return string(body)
	}
	if got := list("dana@example.test"); !strings.Contains(got, `"modelRef":"claude","toolRefs":["github","search"]`) {
		t.Errorf("a member's list: %s, want the refs", got)
	}
	if got := list("eve@example.test"); strings.Contains(got, "modelRef") || strings.Contains(got, "toolRefs") {
		t.Errorf("a viewer's list: %s, want no refs", got)
	}
}

// With a login, the author of a comment, a reply or an approval is the
// caller, whatever the request names.
func TestAuthorIsTheCaller(t *testing.T) {
	m := manager.New(memory.NewBackend().Open(), manager.Config{Agents: manager.ReadyBuiltin(ns, "coder", "claude")})
	mux := http.NewServeMux()
	api.NewServer(m, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), &auth.Identity{Username: "dana@example.test"})))
	}))
	t.Cleanup(srv.Close)
	c := client.New(srv.URL, nil)
	if _, err := c.InitTeam(ctx, ns, api.TeamRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateTask(ctx, ns, "x", v1alpha1.TaskSpec{Title: "x"}); err != nil {
		t.Fatal(err)
	}
	entry, err := c.Comment(ctx, ns, "x", "mallory", "hello")
	if err != nil || entry.Author.Name != "dana@example.test" {
		t.Fatalf("comment as mallory: %+v, %v", entry, err)
	}
}
