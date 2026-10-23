package mcp_test

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/mcp"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
)

const ns = "team-a"

var (
	ctx     = context.Background()
	builtin = map[string]string{v1alpha1.LabelRuntime: string(v1alpha1.RuntimeBuiltin)}
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

type env struct {
	m   *manager.Manager
	srv *httptest.Server
	clk *clock
}

func start(t *testing.T) *env {
	t.Helper()
	store := memory.NewBackend().Open()
	clk := &clock{t: time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)}
	m := manager.New(store, manager.Config{Agents: manager.ReadyBuiltin(ns, "coder", "claude")}, manager.WithClock(clk.now))
	if _, err := m.InitTeam(ctx, ns, v1alpha1.TeamSpec{MaxConcurrentRuns: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateTask(ctx, ns, "t", v1alpha1.TaskSpec{Title: "t", Assignee: &v1alpha1.Assignee{Kind: v1alpha1.AssigneeAgent, Name: "coder"}}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mcp.NewServer(m, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{m: m, srv: srv, clk: clk}
}

func (e *env) claim(t *testing.T) *manager.Claimed {
	t.Helper()
	c, err := e.m.Claim(ctx, manager.ClaimRequest{Runner: "r", Labels: builtin})
	if err != nil || c == nil || c.MCPToken == "" {
		t.Fatalf("claim: %+v, %v", c, err)
	}
	return c
}

// bearer adds the run's token to every request, as a runner does.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// transport is an MCP client transport for the run's endpoint.
func (e *env) transport(token string) *sdk.StreamableClientTransport {
	return &sdk.StreamableClientTransport{
		Endpoint:   e.srv.URL + mcp.Path,
		HTTPClient: &http.Client{Transport: bearer{token: token, next: http.DefaultTransport}},
		MaxRetries: -1,
	}
}

// connect opens an MCP session on the run's endpoint and lists its tools.
func (e *env) connect(token string) ([]*sdk.Tool, error) {
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil)
	session, err := client.Connect(ctx, e.transport(token), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// status posts an MCP initialize with the given Authorization header and
// returns the HTTP status.
func (e *env) status(t *testing.T, authorization string) int {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+mcp.Path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestTheTokenOpensItsRunOnly(t *testing.T) {
	e := start(t)
	c := e.claim(t)
	if tools, err := e.connect(c.MCPToken); err != nil || len(tools) != 0 {
		t.Fatalf("connect with the run's token: %d tools, %v", len(tools), err)
	}

	where, secret, _ := strings.Cut(c.MCPToken, ".")
	tampered := where + "." + strings.Repeat("A", len(secret))
	badNamespace := base64.RawURLEncoding.EncodeToString([]byte("Not_A_Namespace/t")) + "." + secret
	for name, auth := range map[string]string{
		"no header":       "",
		"empty bearer":    "Bearer ",
		"basic auth":      "Basic " + c.MCPToken,
		"tampered secret": "Bearer " + tampered,
		"no secret":       "Bearer " + where,
		"garbage":         "Bearer not-a-token",
		"bad namespace":   "Bearer " + badNamespace,
	} {
		if got := e.status(t, auth); got != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, got)
		}
	}
	if _, err := e.connect(tampered); err == nil {
		t.Error("an MCP client connected with a tampered token")
	}
}

// The token is bound to its run: it dies with a cancel, and a reclaimed task
// gets a fresh token with its new run.
func TestTheTokenDiesWithItsRun(t *testing.T) {
	e := start(t)
	first := e.claim(t)
	e.clk.add(time.Minute) // past the lease and the grace
	if err := e.m.ReclaimExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.status(t, "Bearer "+first.MCPToken); got != http.StatusNotFound {
		t.Fatalf("token of a reclaimed run: status %d, want 404", got)
	}
	second := e.claim(t)
	if second.MCPToken == first.MCPToken {
		t.Fatal("the new run got the old token")
	}
	if _, err := e.connect(second.MCPToken); err != nil {
		t.Fatalf("new run's token: %v", err)
	}
	if _, err := e.m.Cancel(ctx, storage.Key{Namespace: ns, Name: "t"}); err != nil {
		t.Fatal(err)
	}
	if got := e.status(t, "Bearer "+second.MCPToken); got != http.StatusNotFound {
		t.Fatalf("token after cancel: status %d, want 404", got)
	}
}
