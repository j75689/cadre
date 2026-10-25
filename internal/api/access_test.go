package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
)

// fakeAccesses keeps AgentAccesses as the cluster would: versioned, the
// CRD refusing an admin role, and each with a minted token's hash.
type fakeAccesses struct {
	mu        sync.Mutex
	objs      map[string]*v1alpha1.AgentAccess
	rv        int
	rotatedBy []string
}

// tokenHash is what every fake AgentAccess holds as its token's hash: no
// response may carry it.
const tokenHash = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"

func (f *fakeAccesses) ListAgentAccesses(_ context.Context, ns string) ([]v1alpha1.AgentAccess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []v1alpha1.AgentAccess
	for _, aa := range f.objs {
		if aa.Namespace == ns {
			out = append(out, *aa.DeepCopy())
		}
	}
	// By name, as the cluster's store lists them.
	slices.SortFunc(out, func(a, b v1alpha1.AgentAccess) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (f *fakeAccesses) GetAgentAccess(_ context.Context, ns, name string) (*v1alpha1.AgentAccess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if aa, ok := f.objs[ns+"/"+name]; ok {
		return aa.DeepCopy(), nil
	}
	return nil, storage.ErrNotFound
}

func (f *fakeAccesses) write(aa *v1alpha1.AgentAccess) error {
	if aa.Name == "forbidden" {
		return fmt.Errorf("create agentaccesses: %w", api.ErrNotPermitted)
	}
	if aa.Spec.Role != "viewer" && aa.Spec.Role != "member" {
		return fmt.Errorf(`spec.role: Unsupported value: %q: supported values: "member", "viewer": %w`, aa.Spec.Role, storage.ErrInvalid)
	}
	f.rv++
	aa.ResourceVersion, aa.Generation = fmt.Sprint(f.rv), aa.Generation+1
	aa.Status.TokenSecret, aa.Status.TokenHash = aa.Name+".access-token", tokenHash
	f.objs[aa.Namespace+"/"+aa.Name] = aa.DeepCopy()
	return nil
}

func (f *fakeAccesses) CreateAgentAccess(_ context.Context, aa *v1alpha1.AgentAccess) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objs[aa.Namespace+"/"+aa.Name]; ok {
		return storage.ErrAlreadyExists
	}
	return f.write(aa)
}

func (f *fakeAccesses) UpdateAgentAccess(_ context.Context, aa *v1alpha1.AgentAccess) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.objs[aa.Namespace+"/"+aa.Name]
	switch {
	case !ok:
		return storage.ErrNotFound
	case cur.ResourceVersion != aa.ResourceVersion:
		return storage.ErrConflict
	}
	return f.write(aa)
}

// mintedToken is what the fake mints: it may be in the rotate's answer,
// and in no other.
const mintedToken = "dGVhbS1hL2JvdA.minted-by-the-fake"

func (f *fakeAccesses) RotateAgentAccessToken(_ context.Context, ns, name, by string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	aa, ok := f.objs[ns+"/"+name]
	switch {
	case !ok:
		return "", storage.ErrNotFound
	case aa.Spec.ServiceAccount != nil:
		return "", fmt.Errorf("it authenticates as a ServiceAccount: %w", storage.ErrInvalid)
	}
	f.rotatedBy = append(f.rotatedBy, by)
	return mintedToken, nil
}

func (f *fakeAccesses) DeleteAgentAccess(_ context.Context, ns, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objs[ns+"/"+name]; !ok {
		return storage.ErrNotFound
	}
	delete(f.objs, ns+"/"+name)
	return nil
}

// A Team's admins, bootstrap admins among them, manage its AgentAccesses;
// nobody else, an agent included, may even list them; an edit names the
// version it was made from; and no answer carries a token's hash.
func TestAgentAccessRoutes(t *testing.T) {
	store := &fakeAccesses{objs: map[string]*v1alpha1.AgentAccess{}}
	e := startRolesWith(t, memory.NewBackend().Open(), nil, &authz.Policy{AdminUsers: []string{"root@example.test"}, FoldUsernames: true}, api.WithAgentAccesses(store))
	admin := e.as("lead@example.test")
	root := e.as("root@example.test")
	member := e.as("dana@example.test", "backend", "readers")
	stranger := e.as("mallory@example.test", "elsewhere")
	const path = "/namespaces/team-a/agentaccesses"
	var bodies []string
	do := func(token, method, p, body string) (int, string) {
		t.Helper()
		code, out := e.do(token, method, p, body)
		bodies = append(bodies, out)
		return code, out
	}
	for _, c := range []struct {
		name, token, method, path, body string
		code                            int
		says                            string
	}{
		{"an admin makes one", admin, "PUT", path + "/bot", `{"spec":{"role":"member"}}`, 201, `"tokenSecret":"bot.access-token"`},
		{"making it again", admin, "PUT", path + "/bot", `{"spec":{"role":"member"}}`, 409, "already_exists"},
		{"an edit from a stale version", admin, "PUT", path + "/bot", `{"resourceVersion":"0","spec":{"role":"viewer"}}`, 409, "changed since it was read"},
		{"an edit of one gone", admin, "PUT", path + "/gone", `{"resourceVersion":"1","spec":{"role":"viewer"}}`, 409, "deleted since it was read"},
		{"an edit from its version", admin, "PUT", path + "/bot", `{"resourceVersion":"1","spec":{"role":"viewer"}}`, 200, `"role":"viewer"`},
		{"an admin role, which the CRD refuses", admin, "PUT", path + "/boss", `{"spec":{"role":"admin"}}`, 400, "Unsupported value"},
		{"a bootstrap admin makes one", root, "PUT", path + "/ci", `{"spec":{"role":"viewer","serviceAccount":{"name":"ci"}}}`, 201, `"serviceAccount":{"name":"ci"}`},
		{"an admin lists them", admin, "GET", path, "", 200, `"name":"bot"`},
		{"an admin reads one", admin, "GET", path + "/bot", "", 200, `"role":"viewer"`},
		{"an admin reads one there is not", admin, "GET", path + "/none", "", 404, "not_found"},
		{"a member may not read one", member, "GET", path + "/bot", "", 403, "no_role"},
		{"cadre-server's own RBAC refusing", admin, "PUT", path + "/forbidden", `{"spec":{"role":"member"}}`, 500, "server_forbidden"},
		{"a member may not list", member, "GET", path, "", 403, "needs the admin role"},
		{"a member may not make one", member, "PUT", path + "/mine", `{"spec":{"role":"member"}}`, 403, "no_role"},
		{"a member may not delete", member, "DELETE", path + "/bot", "", 403, "no_role"},
		{"a stranger may not list", stranger, "GET", path, "", 403, "no role there"},
		{"an agent, a member, may not list", "bot.member", "GET", path, "", 403, "you are a member"},
		{"another team's", admin, "GET", "/namespaces/team-b/agentaccesses", "", 403, "no role there"},
		{"an admin rotates a token", admin, "POST", path + "/bot/token", "", 200, `"token":"` + mintedToken + `"`},
		{"a member may not rotate one", member, "POST", path + "/bot/token", "", 403, "no_role"},
		{"rotating one there is not", admin, "POST", path + "/none/token", "", 404, "not_found"},
		{"rotating a ServiceAccount's", admin, "POST", path + "/ci/token", "", 400, "ServiceAccount"},
		{"an admin deletes one", admin, "DELETE", path + "/ci", "", 204, ""},
		{"deleting it again", admin, "DELETE", path + "/ci", "", 404, ""},
	} {
		code, out := do(c.token, c.method, c.path, c.body)
		if code != c.code || !strings.Contains(out, c.says) {
			t.Errorf("%s: %d %s, want %d %q", c.name, code, out, c.code, c.says)
		}
	}
	_, out := do(admin, "GET", path, "")
	var list api.AgentAccessList
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list.Items) != 1 || list.Items[0].Name != "bot" || list.Items[0].Reason != "Pending" {
		t.Errorf("list %s", out)
	}
	rotates := 0
	for _, b := range bodies {
		if strings.Contains(b, tokenHash) || strings.Contains(b, "tokenHash") {
			t.Errorf("a response carries the token's hash: %s", b)
		}
		if strings.Contains(b, mintedToken) {
			rotates++
		}
	}
	if rotates != 1 {
		t.Errorf("the minted token is in %d answers, want the rotate's alone", rotates)
	}
	if len(store.rotatedBy) != 1 || store.rotatedBy[0] != "lead@example.test" {
		t.Errorf("rotated by %v, want the admin", store.rotatedBy)
	}
}

// Without logins everyone may do anything: AgentAccesses are not served
// there, since one made then would be a live credential once logins are on.
func TestAgentAccessesNotServedWithoutLogins(t *testing.T) {
	m := manager.New(memory.NewBackend().Open(), manager.Config{})
	mux := http.NewServeMux()
	api.NewServer(m, slog.New(slog.NewTextHandler(io.Discard, nil)), api.WithAgentAccesses(&fakeAccesses{objs: map[string]*v1alpha1.AgentAccess{}})).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		path := "/namespaces/team-a/agentaccesses/bot"
		if method == "GET" {
			path = "/namespaces/team-a/agentaccesses"
		}
		req, _ := http.NewRequest(method, srv.URL+api.Base+path, strings.NewReader(`{"spec":{"role":"member"}}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s without logins: %d", method, path, resp.StatusCode)
		}
	}
}
