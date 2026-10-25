package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
)

// fakeAccesses keeps AgentAccesses as the cluster would: versioned, the
// CRD refusing an admin role, and each with a minted token's hash.
type fakeAccesses struct {
	mu   sync.Mutex
	objs map[string]*v1alpha1.AgentAccess
	rv   int
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
		{"a member may not list", member, "GET", path, "", 403, "needs the admin role"},
		{"a member may not make one", member, "PUT", path + "/mine", `{"spec":{"role":"member"}}`, 403, "no_role"},
		{"a member may not delete", member, "DELETE", path + "/bot", "", 403, "no_role"},
		{"a stranger may not list", stranger, "GET", path, "", 403, "no role there"},
		{"an agent, a member, may not list", "bot.member", "GET", path, "", 403, "you are a member"},
		{"another team's", admin, "GET", "/namespaces/team-b/agentaccesses", "", 403, "no role there"},
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
	for _, b := range bodies {
		if strings.Contains(b, tokenHash) || strings.Contains(b, "tokenHash") {
			t.Errorf("a response carries the token's hash: %s", b)
		}
	}
}
