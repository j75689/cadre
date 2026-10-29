package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage/memory"
)

// configCluster is a cluster's Agents as the controllers' cache holds them:
// writer as kubectl apply leaves it, reviewer edited since it was checked,
// and elsewhere in team-b.
func configCluster(t *testing.T) api.Option {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	writer := manager.ReadyAgent("team-a", "writer", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
	writer.Spec.Instructions = "write the docs"
	writer.Labels = map[string]string{"tier": "one"}
	writer.Annotations = map[string]string{
		"cadre.dev/owner": "docs-team",
		"kubectl.kubernetes.io/last-applied-configuration": `{"spec":{"instructions":"write the docs"}}`,
	}
	writer.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl-client-side-apply", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "cadre.dev/v1alpha1", FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:instructions":{}}}`)}}
	reviewer := manager.ReadyAgent("team-a", "reviewer", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
	reviewer.Generation = 2
	elsewhere := manager.ReadyAgent("team-b", "elsewhere", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin})
	return api.WithConfig(fake.NewClientBuilder().WithScheme(scheme).WithObjects(writer, reviewer, elsewhere).Build())
}

// startConfig serves the API, with Teams in team-a and team-b, to callers
// named by the X-User header, in the groups of X-Groups.
func startConfig(t *testing.T, opts ...api.Option) func(user, groups, path string) (int, string) {
	t.Helper()
	store := memory.NewBackend().Open()
	roles := []v1alpha1.TeamRole{
		{Role: v1alpha1.RoleViewer, Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectGroup, Name: "readers"}}},
		{Role: v1alpha1.RoleMember, Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectGroup, Name: "backend"}}},
		{Role: v1alpha1.RoleAdmin, Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectUser, Name: "lead@example.test"}}},
	}
	for _, n := range []string{"team-a", "team-b"} {
		if err := store.CreateTeam(ctx, &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: n, Name: v1alpha1.TeamName}, Spec: v1alpha1.TeamSpec{Roles: roles}}); err != nil {
			t.Fatal(err)
		}
	}
	m := manager.New(store, manager.Config{Agents: manager.StaticAgents{Agents: map[string]*v1alpha1.Agent{}}})
	mux := http.NewServeMux()
	api.NewServer(m, slog.New(slog.NewTextHandler(io.Discard, nil)), opts...).Register(mux)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := r.Header.Get("X-User"); u != "" {
			id := &auth.Identity{Username: u, Groups: strings.Split(r.Header.Get("X-Groups"), ",")}
			r = r.WithContext(auth.WithIdentity(r.Context(), id))
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return func(user, groups, path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+api.Base+path, nil)
		req.Header.Set("X-User", user)
		req.Header.Set("X-Groups", groups)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		out, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(out)
	}
}

func TestGetAgent(t *testing.T) {
	get := startConfig(t, configCluster(t))
	for _, c := range []struct {
		name, path string
		code       int
	}{
		{"a namespace without a Team", "/namespaces/team-c/agents/writer", 404},
		{"a namespace that cannot be one", "/namespaces/Team_A/agents/writer", 400},
		{"an Agent that is not there", "/namespaces/team-a/agents/nope", 404},
		{"another namespace's Agent", "/namespaces/team-a/agents/elsewhere", 404},
		{"in its own namespace", "/namespaces/team-b/agents/elsewhere", 200},
	} {
		if code, body := get("", "", c.path); code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.code)
		}
	}

	code, body := get("", "", "/namespaces/team-a/agents/reviewer")
	var got api.ConfigResource
	if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
		t.Fatalf("reviewer: %d %s", code, body)
	}
	want := api.ConfigSummary{Name: "reviewer", Reason: "Pending", Message: "its current spec has not been checked yet"}
	if got.ConfigSummary != want || got.Kind != "Agent" || got.Generation != 2 || got.ObservedGeneration != 1 || len(got.Conditions) != 1 {
		t.Errorf("an Agent edited since it was checked: %s", body)
	}
}

// What each role sees of an Agent; nobody sees its fields' owners, nor the
// spec kubectl last applied.
func TestAgentByRole(t *testing.T) {
	policy := &authz.Policy{FoldUsernames: true}
	withRoles := startConfig(t, api.WithAccess(auth.ModeOIDC, policy), configCluster(t))
	withoutLogins := startConfig(t, configCluster(t))
	for _, c := range []struct {
		name                              string
		get                               func(user, groups, path string) (int, string)
		user, groups                      string
		spec, status, annotations, labels bool
	}{
		{"a viewer", withRoles, "eve@example.test", "readers", false, false, false, true},
		{"a member", withRoles, "dana@example.test", "backend", true, true, true, true},
		{"an admin", withRoles, "lead@example.test", "", true, true, true, true},
		{"--auth=none", withoutLogins, "", "", true, true, true, true},
	} {
		code, body := c.get(c.user, c.groups, "/namespaces/team-a/agents/writer")
		var got api.ConfigResource
		if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 {
			t.Fatalf("%s: %d %s", c.name, code, body)
		}
		if !got.Ready || len(got.Conditions) != 1 || got.Metadata.ResourceVersion == "" {
			t.Errorf("%s: no health: %s", c.name, body)
		}
		if (got.Spec != nil) != c.spec || (got.Status != nil) != c.status || (got.Metadata.Annotations != nil) != c.annotations || (got.Metadata.Labels != nil) != c.labels {
			t.Errorf("%s: spec %v, status %v, annotations %v, labels %v: %s", c.name, got.Spec != nil, got.Status != nil, got.Metadata.Annotations != nil, got.Metadata.Labels != nil, body)
		}
		if c.spec && (!strings.Contains(body, "write the docs") || !strings.Contains(body, `"cadre.dev/owner":"docs-team"`)) {
			t.Errorf("%s: spec or annotations missing: %s", c.name, body)
		}
		for _, never := range []string{"last-applied", "managedFields", "f:instructions"} {
			if strings.Contains(body, never) {
				t.Errorf("%s sees %q: %s", c.name, never, body)
			}
		}
	}
	if code, body := withRoles("mallory@example.test", "", "/namespaces/team-a/agents/writer"); code != 403 {
		t.Errorf("a stranger: %d %s", code, body)
	}
}

// Without WithConfig, an Agent is not served, and Agents are still listed.
func TestAgentsWithoutConfig(t *testing.T) {
	get := startConfig(t)
	if code, body := get("", "", "/namespaces/team-a/agents/writer"); code != 404 {
		t.Errorf("an Agent: %d %s", code, body)
	}
	if code, body := get("", "", "/namespaces/team-a/agents"); code != 200 || body != `{"items":[]}`+"\n" {
		t.Errorf("the Agents: %d %q", code, body)
	}
}
