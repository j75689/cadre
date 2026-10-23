package mcp_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/mcp"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
)

// sources serves fixed Agents, ToolServers and Secret values.
type sources struct {
	agent       *v1alpha1.Agent
	toolServers map[string]*v1alpha1.ToolServer
	secrets     map[string]string // "name/key" → value
	// changed holds ToolServers changed since the cache saw them: fresh
	// reads see these.
	changed map[string]*v1alpha1.ToolServer
}

func (s *sources) CachedAgent(context.Context, string, string) (*v1alpha1.Agent, error) {
	return s.agent.DeepCopy(), nil
}

func (s *sources) CachedToolServer(_ context.Context, _, name string) (*v1alpha1.ToolServer, error) {
	if ts, ok := s.toolServers[name]; ok {
		return ts.DeepCopy(), nil
	}
	return nil, fmt.Errorf("toolserver %s: %w", name, storage.ErrNotFound)
}

func (s *sources) ToolServer(ctx context.Context, ns, name string) (*v1alpha1.ToolServer, error) {
	if ts, ok := s.changed[name]; ok {
		return ts.DeepCopy(), nil
	}
	return s.CachedToolServer(ctx, ns, name)
}

func (s *sources) SecretValue(_ context.Context, _ string, ref v1alpha1.SecretKeyRef) (string, error) {
	if v, ok := s.secrets[ref.Name+"/"+ref.Key]; ok {
		return v, nil
	}
	return "", fmt.Errorf("secret: %w", storage.ErrNotFound)
}

func ready(ts *v1alpha1.ToolServer) *v1alpha1.ToolServer {
	ts.Namespace, ts.Generation = ns, 1
	ts.Status = v1alpha1.ConfigStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{
		{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "External"},
	}}
	return ts
}

// upstream is a ToolServer: an MCP server that wants X-API-Key and counts
// how often its tools are listed.
type upstream struct {
	*httptest.Server
	lists   atomic.Int32
	lastArg atomic.Value
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	server := sdk.NewServer(&sdk.Implementation{Name: "upstream", Version: "v0"}, nil)
	type q struct {
		Query string `json:"query"`
	}
	sdk.AddTool(server, &sdk.Tool{Name: "search", Description: "search the code"}, func(_ context.Context, _ *sdk.CallToolRequest, in q) (*sdk.CallToolResult, any, error) {
		u.lastArg.Store(in.Query)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "found " + in.Query}}}, nil, nil
	})
	sdk.AddTool(server, &sdk.Tool{Name: "create_pull_request"}, func(_ context.Context, _ *sdk.CallToolRequest, in q) (*sdk.CallToolResult, any, error) {
		u.lastArg.Store(in.Query)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "opened " + in.Query}}}, nil, nil
	})
	for _, name := range []string{"delete_repo", "weird.name/v2", strings.Repeat("x", 70), "dup.x", "dup/x", "dup_x"} {
		sdk.AddTool(server, &sdk.Tool{Name: name}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "done"}}}, nil, nil
		})
	}
	sdk.AddTool(server, &sdk.Tool{Name: "big"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: strings.Repeat("y", 2<<20)}}}, nil, nil
	})
	sdk.AddTool(server, &sdk.Tool{Name: "runes"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
		// One byte, then two-byte runes past the limit, then more parts.
		return &sdk.CallToolResult{Content: []sdk.Content{
			&sdk.TextContent{Text: "a" + strings.Repeat("é", 1<<20)}, &sdk.TextContent{Text: "more"}, &sdk.TextContent{Text: "more"},
		}}, nil, nil
	})
	sdk.AddTool(server, &sdk.Tool{Name: "picture"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.ImageContent{Data: []byte("png"), MIMEType: "image/png"}, &sdk.TextContent{Text: "a cat"}}}, nil, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true})
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "not-a-real-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if strings.Contains(peek(r), `"tools/list"`) {
			u.lists.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

// peek reads the request body and puts it back.
func peek(r *http.Request) string {
	var b strings.Builder
	data := make([]byte, 0, 512)
	buf := make([]byte, 512)
	for {
		n, err := r.Body.Read(buf)
		data = append(data, buf[:n]...)
		if err != nil {
			break
		}
	}
	b.Write(data)
	r.Body = readCloser{strings.NewReader(b.String())}
	return b.String()
}

type readCloser struct{ *strings.Reader }

func (readCloser) Close() error { return nil }

func forwardingEnv(t *testing.T) (*env, *upstream, *sources) {
	t.Helper()
	up := newUpstream(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close() // a ToolServer nobody answers at
	src := &sources{
		agent: manager.ReadyAgent(ns, "coder", "claude", v1alpha1.AgentRuntime{Type: v1alpha1.RuntimeBuiltin}),
		toolServers: map[string]*v1alpha1.ToolServer{
			"github": ready(&v1alpha1.ToolServer{ObjectMeta: metav1.ObjectMeta{Name: "github"}, Spec: v1alpha1.ToolServerSpec{
				URL:  up.URL,
				Auth: &v1alpha1.ToolServerAuth{SecretRef: v1alpha1.SecretKeyRef{Name: "github", Key: "token"}, Header: "X-API-Key", Prefix: new(string)},
				Permissions: map[string]v1alpha1.Permission{
					"delete_repo": v1alpha1.PermissionDeny, "create_pull_request": v1alpha1.PermissionAsk, "*": v1alpha1.PermissionAllow,
				},
			}}),
			"offline": ready(&v1alpha1.ToolServer{ObjectMeta: metav1.ObjectMeta{Name: "offline"}, Spec: v1alpha1.ToolServerSpec{URL: dead.URL}}),
		},
		secrets: map[string]string{"github/token": "not-a-real-key\n"}, // as a Secret written from a file
	}
	src.agent.Spec.ToolRefs = []string{"github", "offline", "missing"}
	return startWith(t, memory.NewBackend().Open(), mcp.NewForwarder(src, slog.New(slog.NewTextHandler(io.Discard, nil)))), up, src
}

func TestForwardedTools(t *testing.T) {
	e, up, _ := forwardingEnv(t)
	c := e.claim(t)
	s := e.session(t, c.MCPToken)

	tools, err := s.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var forwarded []string
	for _, tool := range tools.Tools {
		if strings.Contains(tool.Name, "__") {
			forwarded = append(forwarded, tool.Name)
		}
	}
	slices.Sort(forwarded)
	// delete_repo is denied, the 70-character name would exceed 64, the three dup tools all
	// become github__dup_x so none is offered, and the offline and missing
	// servers contribute nothing.
	if want := []string{"github__big", "github__create_pull_request", "github__picture", "github__runes", "github__search", "github__weird_name_v2"}; !slices.Equal(forwarded, want) {
		t.Fatalf("forwarded tools %v, want %v", forwarded, want)
	}
	if len(tools.Tools) != 7+len(forwarded) {
		t.Fatalf("%d tools, want the seven core tools and the forwarded ones", len(tools.Tools))
	}

	var res *sdk.CallToolResult
	if res, err = s.CallTool(ctx, &sdk.CallToolParams{Name: "github__search", Arguments: map[string]any{"query": "flaky login"}}); err != nil || res.IsError {
		t.Fatalf("search: %+v, %v", res, err)
	}
	if text := res.Content[0].(*sdk.TextContent).Text; text != "found flaky login" || up.lastArg.Load() != "flaky login" {
		t.Fatalf("search result %q, upstream saw %v", text, up.lastArg.Load())
	}
	if res, err = s.CallTool(ctx, &sdk.CallToolParams{Name: "github__weird_name_v2"}); err != nil || res.IsError {
		t.Fatalf("a sanitised name must reach the original tool: %+v, %v", res, err)
	}

	res, err = s.CallTool(ctx, &sdk.CallToolParams{Name: "github__big"})
	if err != nil || len(res.Content) != 2 || len(res.Content[0].(*sdk.TextContent).Text) != 1<<20 || !strings.Contains(res.Content[1].(*sdk.TextContent).Text, "truncated") {
		t.Fatalf("big result: %v, %d parts", err, len(res.Content))
	}
	res, err = s.CallTool(ctx, &sdk.CallToolParams{Name: "github__runes"})
	if err != nil || len(res.Content) != 2 {
		t.Fatalf("runes result: %v, %d parts; want the cut text and one note", err, len(res.Content))
	}
	if text := res.Content[0].(*sdk.TextContent).Text; len(text) != 1<<20-1 || !utf8.ValidString(text) {
		t.Fatalf("cut text: %d bytes, valid UTF-8 %v; want a cut on the rune boundary", len(text), utf8.ValidString(text))
	}
	res, err = s.CallTool(ctx, &sdk.CallToolParams{Name: "github__picture"})
	if err != nil || len(res.Content) != 2 || res.Content[0].(*sdk.TextContent).Text != "a cat" || !strings.Contains(res.Content[1].(*sdk.TextContent).Text, "dropped") {
		t.Fatalf("picture result: %v, %+v", err, res)
	}

	if res, err := s.CallTool(ctx, &sdk.CallToolParams{Name: "github__delete_repo"}); err == nil && !res.IsError {
		t.Error("a denied tool was called")
	}
}

// Tool lists are cached per ToolServer generation; a new generation lists
// again.
func TestDiscoveryIsCached(t *testing.T) {
	e, up, src := forwardingEnv(t)
	c := e.claim(t)
	for range 3 {
		if _, err := e.connect(c.MCPToken); err != nil {
			t.Fatal(err)
		}
	}
	if n := up.lists.Load(); n != 1 {
		t.Fatalf("upstream listed %d times for three sessions, want 1", n)
	}
	gh := src.toolServers["github"]
	gh.Generation, gh.Status.ObservedGeneration, gh.Status.Conditions[0].ObservedGeneration = 2, 2, 2
	if _, err := e.connect(c.MCPToken); err != nil {
		t.Fatal(err)
	}
	if n := up.lists.Load(); n != 2 {
		t.Fatalf("upstream listed %d times after a new generation, want 2", n)
	}
}

// A ToolServer without a permissions list offers every tool, and one whose
// credential is wrong offers none, without breaking the core tools.
func TestPermissionsAndCredentials(t *testing.T) {
	e, _, src := forwardingEnv(t)
	src.toolServers["github"].Spec.Permissions = nil
	c := e.claim(t)
	tools, err := e.connect(c.MCPToken)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(tools, func(t *sdk.Tool) bool { return t.Name == "github__delete_repo" }) {
		t.Fatal("without permissions every tool must be offered")
	}

	e2, _, src2 := forwardingEnv(t)
	src2.secrets["github/token"] = "wrong"
	c2 := e2.claim(t)
	tools, err = e2.connect(c2.MCPToken)
	if err != nil || len(tools) != 7 {
		t.Fatalf("with a wrong credential: %d tools, %v; want the seven core tools", len(tools), err)
	}
}

// A call reads its ToolServer fresh: a tool denied since the list was built
// is not called.
func TestCallRereadsPermissions(t *testing.T) {
	e, up, src := forwardingEnv(t)
	c := e.claim(t)
	s := e.session(t, c.MCPToken)
	if _, err := s.ListTools(ctx, nil); err != nil {
		t.Fatal(err)
	}
	denied := src.toolServers["github"].DeepCopy()
	denied.Spec.Permissions["search"] = v1alpha1.PermissionDeny
	src.changed = map[string]*v1alpha1.ToolServer{"github": denied}
	res, err := s.CallTool(ctx, &sdk.CallToolParams{Name: "github__search", Arguments: map[string]any{"query": "late"}})
	if err != nil || !res.IsError || up.lastArg.Load() == "late" {
		t.Fatalf("a call denied since listing: %+v, %v", res, err)
	}
}

// A ToolServer that never answers delays a request by the discovery
// timeout at most, and only until its failure is remembered.
func TestUnreachableToolServerIsBounded(t *testing.T) {
	defer mcp.SetDiscoveryTimeoutForTest(300 * time.Millisecond)()
	e, _, src := forwardingEnv(t)
	done := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	t.Cleanup(hang.Close)
	t.Cleanup(func() { close(done) })
	src.toolServers["blackhole"] = ready(&v1alpha1.ToolServer{ObjectMeta: metav1.ObjectMeta{Name: "blackhole"}, Spec: v1alpha1.ToolServerSpec{URL: hang.URL}})
	src.agent.Spec.ToolRefs = append(src.agent.Spec.ToolRefs, "blackhole")
	c := e.claim(t)
	for i, limit := range []time.Duration{2 * time.Second, 200 * time.Millisecond} {
		begin := time.Now()
		tools, err := e.connect(c.MCPToken)
		if err != nil || !slices.ContainsFunc(tools, func(t *sdk.Tool) bool { return t.Name == "github__search" }) {
			t.Fatalf("request %d: %v; the reachable ToolServer's tools must be offered", i, err)
		}
		if took := time.Since(begin); took > limit {
			t.Errorf("request %d took %s, want under %s", i, took, limit)
		}
	}
}

// The credential is not sent to wherever a ToolServer redirects.
func TestCredentialStaysWithItsHost(t *testing.T) {
	e, _, src := forwardingEnv(t)
	var leaked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "" {
			leaked.Add(1)
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(elsewhere.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)
	src.toolServers["github"].Spec.URL = redirect.URL
	c := e.claim(t)
	if _, err := e.connect(c.MCPToken); err != nil {
		t.Fatal(err)
	}
	if n := leaked.Load(); n != 0 {
		t.Fatalf("the credential reached the redirect target %d times", n)
	}
}

// A ToolServer named like a DNS subdomain still has its tools offered.
func TestDottedToolServerName(t *testing.T) {
	e, _, src := forwardingEnv(t)
	gh := src.toolServers["github"]
	gh.Name = "tools.github"
	src.toolServers = map[string]*v1alpha1.ToolServer{"tools.github": gh}
	src.agent.Spec.ToolRefs = []string{"tools.github"}
	c := e.claim(t)
	tools, err := e.connect(c.MCPToken)
	if err != nil || !slices.ContainsFunc(tools, func(t *sdk.Tool) bool { return t.Name == "tools_github__search" }) {
		t.Fatalf("tools of tools.github: %v", err)
	}
}

// Only an object schema of type "object" is registered: the SDK panics on
// anything else.
func TestInputSchemas(t *testing.T) {
	for _, tc := range []struct {
		schema any
		ok     bool
	}{
		{nil, true},
		{map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}, true},
		{map[string]any{}, false},
		{map[string]any{"type": "string"}, false},
		{map[string]any{"$ref": "#/defs/x"}, false},
		{"object", false},
		{[]any{"type", "object"}, false},
	} {
		if err := mcp.ObjectSchemaForTest(tc.schema); (err == nil) != tc.ok {
			t.Errorf("schema %v: %v, want ok=%v", tc.schema, err, tc.ok)
		}
	}
}

// A tool that must ask first records the call and ends the run; approving
// makes the call Cadre-side and records its result for the next run.
func TestAskApproveAndDeny(t *testing.T) {
	e, up, src := forwardingEnv(t)
	key := storage.Key{Namespace: ns, Name: "t"}
	human := manager.Author{Kind: manager.AuthorUser, Name: "alice"}
	ask := func(query string) *sdk.CallToolResult {
		t.Helper()
		c := e.claim(t)
		s := e.session(t, c.MCPToken)
		res, err := s.CallTool(ctx, &sdk.CallToolParams{Name: "github__create_pull_request", Arguments: map[string]any{"query": query}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := ask("fix login")
	if res.IsError || res.Meta[mcp.MetaRunEnded] != true || !strings.Contains(res.Content[0].(*sdk.TextContent).Text, "approval") {
		t.Fatalf("ask: %+v", res)
	}
	got, err := e.m.GetTask(ctx, key)
	if err != nil || got.Status.Phase != v1alpha1.PhaseWaiting || got.Status.Waiting.Reason != v1alpha1.WaitingToolApproval {
		t.Fatalf("after an ask: %+v, %v", got.Status, err)
	}
	entries, err := e.m.ReadThread(ctx, key, got.Status.Waiting.ThreadSeq-1, 1)
	if err != nil || len(entries) != 1 || entries[0].Kind != manager.EntryToolCall || entries[0].Call.Tool != "create_pull_request" ||
		entries[0].Call.ToolServer != "github" || string(entries[0].Call.Arguments) != `{"query":"fix login"}` {
		t.Fatalf("ToolCall entry: %+v, %v", entries, err)
	}

	a, err := e.m.Approve(ctx, key, human, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Task.Status.Phase != v1alpha1.PhaseTodo || a.Decision.Kind != manager.EntryApproved || a.Result == nil ||
		a.Result.Body != "opened fix login" || a.Result.Call.IsError || a.Result.Author.Kind != manager.AuthorSystem {
		t.Fatalf("approval: %+v / %+v / %+v", a.Task.Status, a.Decision, a.Result)
	}

	// A tool denied since it was asked is not called, even when approved.
	ask("again")
	denied := src.toolServers["github"].DeepCopy()
	denied.Spec.Permissions["create_pull_request"] = v1alpha1.PermissionDeny
	src.changed = map[string]*v1alpha1.ToolServer{"github": denied}
	if a, err = e.m.Approve(ctx, key, human, true, ""); err != nil || !a.Result.Call.IsError || !strings.Contains(a.Result.Body, "not made") {
		t.Fatalf("approving a tool denied since: %+v, %v", a, err)
	}
	src.changed = nil

	ask("third")
	if a, err = e.m.Approve(ctx, key, human, false, "not now"); err != nil || a.Decision.Kind != manager.EntryDenied || a.Decision.Body != "not now" || a.Result != nil {
		t.Fatalf("deny: %+v, %v", a, err)
	}
	if up.lastArg.Load() != "fix login" {
		t.Fatalf("the upstream saw %v; only the first, approved call may reach it", up.lastArg.Load())
	}
}

// An ask tool denied since the list was built is refused without recording
// a call, and the run goes on.
func TestAskDeniedSinceListingKeepsTheRun(t *testing.T) {
	e, _, src := forwardingEnv(t)
	c := e.claim(t)
	s := e.session(t, c.MCPToken)
	if _, err := s.ListTools(ctx, nil); err != nil {
		t.Fatal(err)
	}
	denied := src.toolServers["github"].DeepCopy()
	denied.Spec.Permissions["create_pull_request"] = v1alpha1.PermissionDeny
	src.changed = map[string]*v1alpha1.ToolServer{"github": denied}
	res, err := s.CallTool(ctx, &sdk.CallToolParams{Name: "github__create_pull_request", Arguments: map[string]any{"query": "x"}})
	if err != nil || !res.IsError {
		t.Fatalf("an ask tool denied since: %+v, %v", res, err)
	}
	key := storage.Key{Namespace: ns, Name: "t"}
	got, err := e.m.GetTask(ctx, key)
	if err != nil || got.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("after the refused call: %+v, %v", got.Status, err)
	}
	if thread, _ := e.m.ReadThread(ctx, key, 0, 0); len(thread) != 0 {
		t.Fatalf("a refused call was recorded: %+v", thread)
	}
}

// A Forwarder without Sources makes no call and does not panic.
func TestForwarderWithoutSources(t *testing.T) {
	f := mcp.NewForwarder(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	res := f.CallTool(ctx, ns, manager.ToolCall{ID: "x", Name: "github__search", ToolServer: "github", Tool: "search"})
	if !res.IsError || !strings.Contains(res.Body, "not made") {
		t.Fatalf("a call without sources: %+v", res)
	}
}
