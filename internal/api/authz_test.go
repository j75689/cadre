package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	return startRolesWith(t, store, wrap, &authz.Policy{AdminUsers: []string{"Root@Example.test"}, FoldUsernames: true})
}

// startRolesWith is startRolesOn with policy deciding the roles.
func startRolesWith(t *testing.T, store storage.Driver, wrap func(storage.Driver) storage.Driver, policy *authz.Policy, opts ...api.Option) *rolesEnv {
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
		PublicURL: srv.URL, SessionKey: []byte(strings.Repeat("k", 32)), Log: log, Agents: agentTokens})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	authn.Register(mux, api.Base)
	api.NewServer(m, log, append([]api.Option{api.WithAccess(auth.ModeOIDC, policy)}, opts...)...).Register(mux)
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
	e := startRolesWith(t, memory.NewBackend().Open(), nil, &authz.Policy{AdminUsers: []string{"Root@Example.test"}, FoldUsernames: true}, configCluster(t))
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
		{"a viewer reads an agent's health", viewer, "GET", "/namespaces/team-a/agents/writer", "", 200, `"ready":true`},
		{"a stranger may not read an agent", stranger, "GET", "/namespaces/team-a/agents/writer", "", 403, "no role there"},
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
	// An Agent's spec is a member's to see.
	if _, out := e.do(viewer, "GET", "/namespaces/team-a/agents/writer", ""); strings.Contains(out, `"spec"`) {
		t.Errorf("a viewer reads an agent's spec: %s", out)
	}
	if _, out := e.do(member, "GET", "/namespaces/team-a/agents/writer", ""); !strings.Contains(out, `"spec"`) {
		t.Errorf("a member does not read an agent's spec: %s", out)
	}
}

// agentTokens are two agents' tokens: a member of team-a, and a viewer
// there whom a test's --admin-users names too.
var agentTokens = staticAgents{
	"bot.member": auth.AgentIdentity("team-a", "bot", "member"),
	"root.agent": auth.AgentIdentity("team-a", "root", "viewer"),
}

type staticAgents map[string]*auth.Identity

func (s staticAgents) Authenticate(_ context.Context, tok string) (*auth.Identity, error) {
	if id, ok := s[tok]; ok {
		return id, nil
	}
	return nil, auth.ErrUnauthenticated
}

// An agent has its AgentAccess's role on its own namespace's team, and on
// no other; no Team role and no bootstrap admin entry gives it more; it
// writes as itself; and GET /me says so.
func TestAgentRoles(t *testing.T) {
	e := startRolesWith(t, memory.NewBackend().Open(), nil, &authz.Policy{AdminUsers: []string{"agent:team-a/root"}}, configCluster(t))
	comment := `{"body":"hello"}`
	for _, c := range []struct {
		name, token, method, path, body string
		code                            int
		says                            string
	}{
		{"a member agent comments", "bot.member", "POST", "/namespaces/team-a/tasks/x/comments", comment, 201, ""},
		{"a member agent creates", "bot.member", "POST", "/namespaces/team-a/tasks", `{"spec":{"title":"y"}}`, 201, ""},
		{"a member agent may not manage the team", "bot.member", "PUT", "/namespaces/team-a/team", "{}", 403, "needs the admin role"},
		{"no role on team-b", "bot.member", "GET", "/namespaces/team-b/tasks/x", "", 403, "no role there"},
		{"no team created", "bot.member", "PUT", "/namespaces/team-c/team", "{}", 403, ""},
		{"named in --admin-users, still a viewer", "root.agent", "POST", "/namespaces/team-a/tasks/x/comments", comment, 403, "you are a viewer"},
		{"named in --admin-users, nothing elsewhere", "root.agent", "GET", "/namespaces/team-b/tasks/x", "", 403, "no role there"},
	} {
		code, out := e.do(c.token, c.method, c.path, c.body)
		if code != c.code || !strings.Contains(out, c.says) {
			t.Errorf("%s: %d %s, want %d %q", c.name, code, out, c.code, c.says)
		}
	}
	if _, out := e.do("bot.member", "GET", "/namespaces/team-a/tasks/x/thread", ""); !strings.Contains(out, `"name":"agent:team-a/bot"`) {
		t.Errorf("thread %s", out)
	}
	// A viewer AgentAccess reads an Agent's health, not its spec.
	if code, out := e.do("root.agent", "GET", "/namespaces/team-a/agents/writer", ""); code != 200 || strings.Contains(out, `"spec"`) {
		t.Errorf("a viewer agent reads an agent: %d %s", code, out)
	}
	if code, out := e.do("bot.member", "GET", "/namespaces/team-a/agents/writer", ""); code != 200 || !strings.Contains(out, `"spec"`) {
		t.Errorf("a member agent reads an agent: %d %s", code, out)
	}
	code, out := e.do("root.agent", "GET", "/me", "")
	var me auth.Me
	if err := json.Unmarshal([]byte(out), &me); err != nil || code != 200 {
		t.Fatalf("me: %d %s", code, out)
	}
	if me.Identity == nil || me.Username != "agent:team-a/root" || me.Agent == nil || me.Agent.Namespace != "team-a" || me.Agent.Role != "viewer" ||
		me.Admin || len(me.Roles) != 1 || me.Roles["team-a"] != "viewer" {
		t.Errorf("me %s", out)
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

// A team's admin edits its Team through the API, from the version read;
// others may read it, without its roles.
func TestTeamEdits(t *testing.T) {
	e := startRoles(t)
	lead, member, root := e.as("lead@example.test"), e.as("dev@example.test", "backend"), e.as("root@example.test")

	read := func(token string) v1alpha1.Team {
		t.Helper()
		code, body := e.do(token, "GET", "/namespaces/team-a/team", "")
		var team v1alpha1.Team
		if code != 200 || json.Unmarshal([]byte(body), &team) != nil {
			t.Fatalf("GET team: %d %s", code, body)
		}
		return team
	}
	if team := read(member); len(team.Spec.Roles) != 0 || len(team.Annotations) != 0 {
		t.Errorf("a member reads the roles: %+v %v", team.Spec.Roles, team.Annotations)
	}
	team := read(lead)
	if len(team.Spec.Roles) != 3 || team.ResourceVersion == "" {
		t.Fatalf("the admin reads %+v, version %q", team.Spec.Roles, team.ResourceVersion)
	}

	edit := func(token, version string, roles []v1alpha1.TeamRole) (int, string) {
		t.Helper()
		body, _ := json.Marshal(api.TeamRequest{TeamSpec: v1alpha1.TeamSpec{DisplayName: "Team A", MaxConcurrentRuns: 3, ArchiveAfter: &metav1.Duration{Duration: 720 * time.Hour}, Roles: roles}, ResourceVersion: version})
		return e.do(token, "PUT", "/namespaces/team-a/team", string(body))
	}
	roles := append(slices.Clone(team.Spec.Roles), v1alpha1.TeamRole{Role: v1alpha1.RoleMember, Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectUser, Name: "new@example.test"}}})
	if code, body := edit(lead, team.ResourceVersion, roles); code != 200 {
		t.Fatalf("the admin's edit: %d %s", code, body)
	}
	got := read(lead)
	if got.Spec.DisplayName != "Team A" || got.Spec.MaxConcurrentRuns != 3 || got.Spec.ArchiveAfter.Duration != 720*time.Hour || len(got.Spec.Roles) != 4 {
		t.Errorf("after the edit: %+v", got.Spec)
	}
	for _, tc := range []struct {
		name, token, version string
		roles                []v1alpha1.TeamRole
		code                 int
		error                string
	}{
		{"an edit from the version before", lead, team.ResourceVersion, roles, 409, "conflict"},
		{"an edit with no version, which creates", lead, "", roles, 409, "already_exists"},
		{"a member's edit", member, got.ResourceVersion, roles, 403, "no_role"},
		{"a role that does not exist", lead, got.ResourceVersion, []v1alpha1.TeamRole{{Role: "owner", Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectUser, Name: "x"}}}}, 400, "invalid"},
		{"a role with no subjects", lead, got.ResourceVersion, []v1alpha1.TeamRole{{Role: v1alpha1.RoleViewer}}, 400, "invalid"},
	} {
		if code, body := edit(tc.token, tc.version, tc.roles); code != tc.code || !strings.Contains(body, `"`+tc.error+`"`) {
			t.Errorf("%s: %d %s, want %d %s", tc.name, code, body, tc.code, tc.error)
		}
	}
	// An edit of a Team gone since it was read is a conflict, not a creation.
	body, _ := json.Marshal(api.TeamRequest{TeamSpec: v1alpha1.TeamSpec{Roles: roles}, ResourceVersion: got.ResourceVersion})
	if code, out := e.do(root, "PUT", "/namespaces/team-c/team", string(body)); code != 409 || !strings.Contains(out, `"conflict"`) || !strings.Contains(out, "deleted since it was read") {
		t.Errorf("an edit of a Team that is not there: %d %s", code, out)
	}
	if code, out := e.do(root, "GET", "/namespaces/team-c/team", ""); code != 404 {
		t.Errorf("the edit made the Team: %d %s", code, out)
	}
	// With a bootstrap admin to give it back, the last admin role may go.
	noAdmin := []v1alpha1.TeamRole{{Role: v1alpha1.RoleMember, Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectGroup, Name: "backend"}}}}
	if code, body := edit(root, got.ResourceVersion, noAdmin); code != 200 {
		t.Errorf("the bootstrap admin taking the admin role away: %d %s", code, body)
	}
}

// Without bootstrap admins, nobody could give a Team an admin back: the
// last admin role stays.
func TestTeamKeepsAnAdmin(t *testing.T) {
	e := startRolesWith(t, memory.NewBackend().Open(), nil, &authz.Policy{FoldUsernames: true})
	lead := e.as("lead@example.test")
	code, body := e.do(lead, "GET", "/namespaces/team-a/team", "")
	var team v1alpha1.Team
	if code != 200 || json.Unmarshal([]byte(body), &team) != nil {
		t.Fatalf("GET team: %d %s", code, body)
	}
	put := func(roles []v1alpha1.TeamRole) (int, string) {
		b, _ := json.Marshal(api.TeamRequest{TeamSpec: v1alpha1.TeamSpec{Roles: roles}, ResourceVersion: team.ResourceVersion})
		return e.do(lead, "PUT", "/namespaces/team-a/team", string(b))
	}
	if code, body := put(team.Spec.Roles[:2]); code != 400 || !strings.Contains(body, "no admin") {
		t.Errorf("removing the last admin role: %d %s", code, body)
	}
	// An admin role with no subjects is told what is wrong with it.
	if code, body := put([]v1alpha1.TeamRole{{Role: v1alpha1.RoleAdmin}}); code != 400 || !strings.Contains(body, "at least one subject") {
		t.Errorf("an admin role with no subjects: %d %s", code, body)
	}
	if code, body := put(team.Spec.Roles); code != 200 {
		t.Errorf("keeping it: %d %s", code, body)
	}
}
