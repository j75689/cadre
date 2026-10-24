package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/auth/oidctest"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
)

// rolesEnv is cadre-server with logins and roles: team-a gives roles by
// group and user, team-b gives none, and root is a bootstrap admin.
type rolesEnv struct {
	t   *testing.T
	url string
	idp *oidctest.Provider
}

// stale serves Teams from an index that lags the store: what the cache says
// is not what the store does.
type stale struct {
	storage.Driver
	teams map[string]*v1alpha1.Team
}

func (s stale) IndexedTeam(_ context.Context, ns string) (*v1alpha1.Team, error) {
	if t, ok := s.teams[ns]; ok {
		return t.DeepCopy(), nil
	}
	return nil, storage.ErrNotFound
}

func startRoles(t *testing.T) *rolesEnv {
	t.Helper()
	return startRolesOn(t, memory.NewBackend().Open(), nil)
}

func startRolesOn(t *testing.T, store storage.Driver, wrap func(storage.Driver) storage.Driver) *rolesEnv {
	t.Helper()
	m := manager.New(store, manager.Config{})
	ctx := context.Background()
	roles := []v1alpha1.TeamRole{
		{Role: v1alpha1.RoleViewer, Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectGroup, Name: "readers"}}},
		{Role: v1alpha1.RoleMember, Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectGroup, Name: "backend"}}},
		{Role: v1alpha1.RoleAdmin, Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectUser, Name: "lead@example.test"}}},
	}
	// team-a as kubectl apply leaves it: the last spec applied, roles and
	// all, in an annotation, and the fields' owners.
	applied, _ := json.Marshal(map[string]any{"spec": map[string]any{"roles": roles}})
	meta := map[string]metav1.ObjectMeta{
		"team-a": {Namespace: "team-a", Name: v1alpha1.TeamName, Labels: map[string]string{"tier": "one"},
			Annotations:   map[string]string{"kubectl.kubernetes.io/last-applied-configuration": string(applied)},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl-client-side-apply", FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:roles":{}}}`)}}},
		"team-b": {Namespace: "team-b", Name: v1alpha1.TeamName},
	}
	for ns, spec := range map[string]v1alpha1.TeamSpec{"team-a": {Roles: roles}, "team-b": {}} {
		if err := store.CreateTeam(ctx, &v1alpha1.Team{ObjectMeta: meta[ns], Spec: spec}); err != nil {
			t.Fatal(err)
		}
	}
	for _, ns := range []string{"team-a", "team-b"} {
		if _, err := m.CreateTask(ctx, ns, "x", v1alpha1.TaskSpec{Title: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if wrap != nil {
		m = manager.New(wrap(store), manager.Config{})
	}
	idp := oidctest.New(t)
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authn, err := auth.NewOIDC(ctx, auth.Config{Issuer: idp.URL, ClientID: oidctest.ClientID, UsernameClaim: "email", GroupsClaim: "groups",
		PublicURL: srv.URL, SessionKey: []byte(strings.Repeat("k", 32)), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	authn.Register(mux, api.Base)
	api.NewServer(m, log, api.WithAccess(auth.ModeOIDC, &authz.Policy{AdminUsers: []string{"Root@Example.test"}, FoldUsernames: true})).Register(mux)
	srv.Config.Handler = authn.Wrap(mux, api.Base)
	return &rolesEnv{t: t, url: srv.URL, idp: idp}
}

// as is a token for a user in groups.
func (e *rolesEnv) as(user string, groups ...string) string {
	e.idp.SetClaim("email", user)
	e.idp.SetClaim("groups", groups)
	return e.idp.IDToken(oidctest.ClientID, "", 0)
}

// do sends a request with token and returns the status and body.
func (e *rolesEnv) do(token, method, path, body string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.url+api.Base+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(out)
}

func TestRoles(t *testing.T) {
	e := startRoles(t)
	viewer := e.as("eve@example.test", "readers")
	member := e.as("dana@example.test", "backend", "readers")
	admin := e.as("lead@example.test")
	stranger := e.as("mallory@example.test", "elsewhere")
	root := e.as("root@example.test")
	comment := `{"body":"hello"}`
	for _, c := range []struct {
		name, token, method, path, body string
		code                            int
		says                            string
	}{
		{"a viewer reads", viewer, "GET", "/namespaces/team-a/tasks/x", "", 200, ""},
		{"a viewer lists", viewer, "GET", "/namespaces/team-a/tasks", "", 200, ""},
		{"a viewer reads the thread", viewer, "GET", "/namespaces/team-a/tasks/x/thread", "", 200, ""},
		{"a viewer lists agents", viewer, "GET", "/namespaces/team-a/agents", "", 200, ""},
		{"a viewer may not comment", viewer, "POST", "/namespaces/team-a/tasks/x/comments", comment, 403, "you are a viewer"},
		{"a viewer may not create", viewer, "POST", "/namespaces/team-a/tasks", `{"spec":{"title":"y"}}`, 403, "needs the member role"},
		{"a viewer may not cancel", viewer, "POST", "/namespaces/team-a/tasks/x/cancel", "{}", 403, ""},
		{"a member comments", member, "POST", "/namespaces/team-a/tasks/x/comments", comment, 201, ""},
		{"a member creates", member, "POST", "/namespaces/team-a/tasks", `{"spec":{"title":"y"}}`, 201, ""},
		{"a member may not manage the team", member, "PUT", "/namespaces/team-a/team", "{}", 403, "needs the admin role"},
		{"an admin reaches the team", admin, "PUT", "/namespaces/team-a/team", "{}", 409, "already"},
		{"a stranger is refused, not told", stranger, "GET", "/namespaces/team-a/tasks/x", "", 403, "no role there"},
		{"a stranger on a missing task", stranger, "GET", "/namespaces/team-a/tasks/nope", "", 403, ""},
		{"a missing task where one may look", viewer, "GET", "/namespaces/team-a/tasks/nope", "", 404, ""},
		{"no role on team-b", member, "GET", "/namespaces/team-b/tasks/x", "", 403, ""},
		{"a namespace without a team", member, "GET", "/namespaces/team-c/tasks/x", "", 403, ""},
		{"a bootstrap admin anywhere", root, "POST", "/namespaces/team-b/tasks/x/comments", comment, 201, ""},
		{"a bootstrap admin on a missing team", root, "GET", "/namespaces/team-c/tasks/x", "", 404, ""},
		{"only a bootstrap admin creates a team", admin, "PUT", "/namespaces/team-c/team", "{}", 403, ""},
		{"a bootstrap admin creates a team", root, "PUT", "/namespaces/team-c/team", "{}", 201, ""},
	} {
		code, out := e.do(c.token, c.method, c.path, c.body)
		if code != c.code || !strings.Contains(out, c.says) {
			t.Errorf("%s: %d %s, want %d %q", c.name, code, out, c.code, c.says)
		}
	}
	// The member's comment is theirs.
	if _, out := e.do(viewer, "GET", "/namespaces/team-a/tasks/x/thread", ""); !strings.Contains(out, `"name":"dana@example.test"`) {
		t.Errorf("thread %s", out)
	}
}

func TestTeamsAndMeFollowRoles(t *testing.T) {
	e := startRoles(t)
	teams := func(token string) []string {
		_, out := e.do(token, "GET", "/teams", "")
		var list api.TeamList
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tm := range list.Items {
			names = append(names, tm.Namespace)
		}
		return names
	}
	if got := teams(e.as("dana@example.test", "backend")); strings.Join(got, ",") != "team-a" {
		t.Errorf("a member of team-a sees %v", got)
	}
	if got := teams(e.as("mallory@example.test")); len(got) != 0 {
		t.Errorf("a stranger sees %v", got)
	}
	if got := teams(e.as("root@example.test")); len(got) != 2 {
		t.Errorf("a bootstrap admin sees %v", got)
	}
	me := func(token string) auth.Me {
		_, out := e.do(token, "GET", "/me", "")
		var m auth.Me
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := me(e.as("dana@example.test", "backend", "readers")); m.Admin || len(m.Roles) != 1 || m.Roles["team-a"] != "member" || m.Username != "dana@example.test" {
		t.Errorf("/me of a member: %+v", m)
	}
	if m := me(e.as("root@example.test")); !m.Admin || m.Roles["team-a"] != "admin" || m.Roles["team-b"] != "admin" {
		t.Errorf("/me of a bootstrap admin: %+v", m)
	}
}

// A role taken away fences the next write at once, though the index still
// has it; reads follow the index.
func TestWritesReadTheTeamLive(t *testing.T) {
	cached := map[string]*v1alpha1.Team{"team-b": {
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: v1alpha1.TeamName},
		Spec:       v1alpha1.TeamSpec{Roles: []v1alpha1.TeamRole{{Role: v1alpha1.RoleMember, Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectUser, Name: "dana@example.test"}}}}},
	}}
	e := startRolesOn(t, memory.NewBackend().Open(), func(d storage.Driver) storage.Driver { return stale{Driver: d, teams: cached} })
	dana := e.as("dana@example.test")
	if code, out := e.do(dana, "GET", "/namespaces/team-b/tasks/x", ""); code != 200 {
		t.Errorf("a read on the index's word: %d %s", code, out)
	}
	if code, out := e.do(dana, "POST", "/namespaces/team-b/tasks/x/comments", `{"body":"hi"}`); code != 403 || !strings.Contains(out, `"no_role"`) {
		t.Errorf("a write the store no longer allows: %d %s", code, out)
	}
}

func TestTeamsHideWhoHoldsRoles(t *testing.T) {
	e := startRoles(t)
	// Nowhere in what a member gets, metadata included.
	_, body := e.do(e.as("dana@example.test", "backend"), "GET", "/teams", "")
	for _, secret := range []string{"lead@example.test", "readers", "last-applied", "f:roles"} {
		if strings.Contains(body, secret) {
			t.Errorf("a member's /teams holds %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, `"tier":"one"`) {
		t.Errorf("labels went too: %s", body)
	}
	if _, body := e.do(e.as("lead@example.test"), "GET", "/teams", ""); !strings.Contains(body, "last-applied") {
		t.Errorf("the team's admin lost the annotations: %s", body)
	}
	roles := func(token string) (bool, bool) {
		_, out := e.do(token, "GET", "/teams", "")
		var list api.TeamList
		if err := json.Unmarshal([]byte(out), &list); err != nil || len(list.Items) == 0 {
			t.Fatalf("%s: %v", out, err)
		}
		return list.Items[0].Spec.Roles != nil, list.Items[0].Status.NextNumber != 0
	}
	if r, s := roles(e.as("dana@example.test", "backend")); r || s {
		t.Errorf("a member sees roles %v, status %v", r, s)
	}
	if r, s := roles(e.as("lead@example.test")); !r || !s {
		t.Errorf("the team's admin sees roles %v, status %v", r, s)
	}
}

// Addresses match without case, in roles and in --admin-users.
func TestUsernamesFoldCase(t *testing.T) {
	e := startRoles(t)
	if code, out := e.do(e.as("LEAD@example.test"), "GET", "/me", ""); code != 200 || !strings.Contains(out, `"team-a":"admin"`) || !strings.Contains(out, `"username":"lead@example.test"`) {
		t.Errorf("an address in capitals: %d %s", code, out)
	}
	if code, out := e.do(e.as("root@EXAMPLE.test"), "GET", "/me", ""); !strings.Contains(out, `"admin":true`) {
		t.Errorf("a bootstrap admin in capitals: %d %s", code, out)
	}
}

func TestMeWithoutLogins(t *testing.T) {
	m := manager.New(memory.NewBackend().Open(), manager.Config{})
	mux := http.NewServeMux()
	api.NewServer(m, slog.New(slog.NewTextHandler(io.Discard, nil)), api.WithAccess(auth.ModeNone, nil)).Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, api.Base+"/me", nil))
	if !strings.Contains(w.Body.String(), `"mode":"none"`) || !strings.Contains(w.Body.String(), `"admin":true`) {
		t.Errorf("/me without logins: %s", w.Body.String())
	}
}

// counting counts reads of the Team from the store.
type counting struct {
	storage.Driver
	live *atomic.Int64
}

func (c counting) GetTeam(ctx context.Context, ns string) (*v1alpha1.Team, error) {
	c.live.Add(1)
	return c.Driver.GetTeam(ctx, ns)
}

// Someone the index refuses costs no store read; a write it allows is
// confirmed on one.
func TestRefusalsReadNoStore(t *testing.T) {
	var live atomic.Int64
	e := startRolesOn(t, memory.NewBackend().Open(), func(d storage.Driver) storage.Driver { return counting{Driver: d, live: &live} })
	for range 20 {
		if code, _ := e.do(e.as("mallory@example.test"), "POST", "/namespaces/team-a/tasks/x/comments", `{"body":"spam"}`); code != 403 {
			t.Fatalf("a stranger's comment: %d", code)
		}
	}
	if n := live.Load(); n != 0 {
		t.Errorf("a stranger's refused writes read the store %d times", n)
	}
	if code, _ := e.do(e.as("dana@example.test", "backend"), "POST", "/namespaces/team-a/tasks/x/comments", `{"body":"hi"}`); code != 201 || live.Load() != 1 {
		t.Errorf("a member's comment: %d, %d store reads", code, live.Load())
	}
	if code, _ := e.do(e.as("dana@example.test", "backend"), "GET", "/namespaces/team-a/tasks/x", ""); code != 200 || live.Load() != 1 {
		t.Errorf("a read: %d, %d store reads", code, live.Load())
	}
}

func TestLoginsNeedAPolicy(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("WithAccess(oidc, nil) accepted")
		}
	}()
	api.NewServer(manager.New(memory.NewBackend().Open(), manager.Config{}), slog.New(slog.NewTextHandler(io.Discard, nil)), api.WithAccess(auth.ModeOIDC, nil))
}
