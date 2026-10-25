package mcp_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/auth/oidctest"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/mcp"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
)

// boardEnv is cadre-server's board endpoint with logins: team-a gives
// readers the viewer role and backend the member role.
type boardEnv struct {
	url   string
	idp   *oidctest.Provider
	store storage.Driver
}

func startBoard(t *testing.T) *boardEnv {
	t.Helper()
	store := memory.NewBackend().Open()
	m := manager.New(store, manager.Config{})
	roles := []v1alpha1.TeamRole{
		{Role: v1alpha1.RoleViewer, Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectGroup, Name: "readers"}}},
		{Role: v1alpha1.RoleMember, Subjects: []v1alpha1.RoleSubject{{Kind: v1alpha1.SubjectGroup, Name: "backend"}}},
	}
	if _, err := m.InitTeam(ctx, ns, v1alpha1.TeamSpec{Roles: roles}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateTask(ctx, ns, "x", v1alpha1.TaskSpec{Title: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := m.NumberNew(ctx); err != nil {
		t.Fatal(err)
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
	mcp.NewBoard(m, log, &authz.Policy{FoldUsernames: true}).Register(mux, authn)
	srv.Config.Handler = mux
	return &boardEnv{url: srv.URL, idp: idp, store: store}
}

// as is an ID token for a user in groups.
func (e *boardEnv) as(user string, groups ...string) string {
	e.idp.SetClaim("email", user)
	e.idp.SetClaim("groups", groups)
	return e.idp.IDToken(oidctest.ClientID, "", 0)
}

// session opens an MCP session on the board endpoint with token.
func (e *boardEnv) session(t *testing.T, token string) *sdk.ClientSession {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil)
	s, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   e.url + mcp.BoardPath,
		HTTPClient: &http.Client{Transport: bearer{token: token, next: http.DefaultTransport}},
		MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// args are a valid call of each board tool on team-a's task x.
var args = map[string]map[string]any{
	"list_tasks":  {"namespace": ns},
	"get_task":    {"namespace": ns, "task": "#1"},
	"create_task": {"namespace": ns, "title": "from an agent"},
	"comment":     {"namespace": ns, "task": "x", "body": "a note"},
	"reply":       {"namespace": ns, "task": "x", "body": "an answer"},
}

// Every board tool is decided by the caller's role on the team it names:
// a viewer reads, a member also writes, a stranger does nothing; and what is
// written is the caller's.
func TestBoardTools(t *testing.T) {
	e := startBoard(t)
	viewer := e.session(t, e.as("ro@example.test", "readers"))

	// The tools served are the ones checked, no more and no fewer.
	res, err := viewer.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var served []string
	for _, tool := range res.Tools {
		served = append(served, tool.Name)
	}
	var checked []string
	for name := range mcp.BoardTools {
		checked = append(checked, name)
		if args[name] == nil {
			t.Fatalf("no test call for %s", name)
		}
	}
	slices.Sort(served)
	slices.Sort(checked)
	if !slices.Equal(served, checked) {
		t.Fatalf("served %v, checked %v", served, checked)
	}

	for _, who := range []struct {
		name    string
		session *sdk.ClientSession
		may     authz.Role
	}{
		{"a viewer", viewer, authz.Viewer},
		{"a member", e.session(t, e.as("dev@example.test", "backend")), authz.Member},
		{"a stranger", e.session(t, e.as("stranger@example.test", "elsewhere")), authz.NoRole},
	} {
		for tool, action := range mcp.BoardTools {
			res, err := who.session.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args[tool]})
			if err != nil {
				t.Fatalf("%s calling %s: %v", who.name, tool, err)
			}
			refused := res.IsError && strings.Contains(text(res), "needs the")
			if want := !authz.Allowed(who.may, action); refused != want {
				t.Errorf("%s calling %s: refused %v, want %v: %s", who.name, tool, refused, want, text(res))
			}
		}
	}

	// The member's comment and task are theirs, whatever a client says.
	thread, err := manager.New(e.store, manager.Config{}).ReadThread(ctx, storage.Key{Namespace: ns, Name: "x"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(thread) != 1 || thread[0].Author.Name != "dev@example.test" || thread[0].Body != "a note" {
		t.Errorf("thread %+v", thread)
	}
}

// The board endpoint takes a bearer ID token and nothing else: no token, a
// bad one, or the API's session cookie is a 401.
func TestBoardNeedsABearerToken(t *testing.T) {
	e := startBoard(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	for name, set := range map[string]func(*http.Request){
		"no token":  func(*http.Request) {},
		"a bad one": func(r *http.Request) { r.Header.Set("Authorization", "Bearer not-a-token") },
		"a cookie":  func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "cadre_session", Value: "x"}) },
	} {
		req, _ := http.NewRequest(http.MethodPost, e.url+mcp.BoardPath, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		set(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d", name, resp.StatusCode)
		}
	}
	// A valid one reads the board, task x by its number among them.
	res, err := e.session(t, e.as("ro@example.test", "readers")).CallTool(ctx, &sdk.CallToolParams{Name: "get_task", Arguments: args["get_task"]})
	if err != nil || res.IsError {
		t.Fatalf("get_task: %v %s", err, text(res))
	}
	var out struct {
		Task v1alpha1.Task `json:"task"`
	}
	if b, _ := json.Marshal(res.StructuredContent); json.Unmarshal(b, &out) != nil || out.Task.Name != "x" {
		t.Errorf("get_task #1: %+v", res.StructuredContent)
	}
}

// text is a tool result's text.
func text(res *sdk.CallToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
