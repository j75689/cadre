package controller_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/auth/oidctest"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/controller"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/mcp"
	"github.com/j75689/cadre/internal/runner"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
	"github.com/j75689/cadre/internal/testenv"
)

// Agents outside Cadre on the board, /mcp, as cadre-server wires them: an
// AgentAccess's ServiceAccount, or its minted token, acts as the
// AgentAccess, with its role on its own namespace's team; a ServiceAccount
// no AgentAccess holds, a Runner's or cadre-runner's, gets nothing; and a
// minted token stops once its Secret is deleted.
func TestAgentAccessesOnTheBoard(t *testing.T) {
	cfg := testenv.APIServer(t)
	mgr, err := controller.NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Setup(mgr, controller.Options{Resync: 200 * time.Millisecond}, controller.SkipNameValidation()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
	c, err := client.New(cfg, client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		t.Fatal(err)
	}
	cs := kubernetes.NewForConfigOrDie(cfg)
	create := func(objs ...client.Object) {
		t.Helper()
		for _, o := range objs {
			if err := c.Create(ctx, o); err != nil {
				t.Fatalf("create %T %s: %v", o, o.GetName(), err)
			}
		}
	}
	for _, ns := range []string{"team-a", "team-b", "cadre-system"} {
		create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	}
	sa := func(ns, name string) *corev1.ServiceAccount {
		return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	}
	create(
		&v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: v1alpha1.TeamName}},
		&v1alpha1.AgentAccess{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"}, Spec: v1alpha1.AgentAccessSpec{Role: "member"}},
		&v1alpha1.AgentAccess{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "ci-bot"}, Spec: v1alpha1.AgentAccessSpec{
			Role: "member", ServiceAccount: &v1alpha1.ServiceAccountRef{Name: "ci-bot"}}},
		&v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "laptop"}, Spec: v1alpha1.RunnerSpec{
			ServiceAccount: &v1alpha1.ServiceAccountRef{Name: "runner"}, Labels: map[string]string{"pool": "x"}}},
		sa("team-a", "ci-bot"), sa("team-a", "runner"), sa("cadre-system", "cadre-runner"),
	)
	// A second AgentAccess naming ci-bot's ServiceAccount, a viewer: the
	// account acts as ci-bot, the older, never as this one.
	time.Sleep(1100 * time.Millisecond)
	create(&v1alpha1.AgentAccess{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "dup"}, Spec: v1alpha1.AgentAccessSpec{
		Role: "viewer", ServiceAccount: &v1alpha1.ServiceAccountRef{Name: "ci-bot"}}})
	saToken := func(ns, name string) string {
		t.Helper()
		tr, err := cs.CoreV1().ServiceAccounts(ns).CreateToken(ctx, name, &authnv1.TokenRequest{Spec: authnv1.TokenRequestSpec{Audiences: []string{controller.RunnerAudience}}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return tr.Status.Token
	}
	minted := func(old string) string {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			var s corev1.Secret
			if c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "bot.access-token"}, &s) == nil && string(s.Data["token"]) != old {
				return string(s.Data["token"])
			}
		}
		t.Fatal("no token was minted")
		return ""
	}

	// The board: a store with team-a's and team-b's boards, logins, and
	// AgentAccesses as cadre-server has them.
	store := memory.NewBackend().Open()
	m := manager.New(store, manager.Config{})
	for _, ns := range []string{"team-a", "team-b"} {
		if _, err := m.InitTeam(ctx, ns, v1alpha1.TeamSpec{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.CreateTask(ctx, "team-a", "x", v1alpha1.TaskSpec{Title: "x"}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	idp := oidctest.New(t)
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	authn, err := auth.NewOIDC(ctx, auth.Config{Issuer: idp.URL, ClientID: oidctest.ClientID, UsernameClaim: "email",
		PublicURL: srv.URL, SessionKey: []byte(strings.Repeat("k", 32)), Log: log,
		Agents: controller.AgentAccesses(mgr.GetClient(), mgr.GetAPIReader(), runner.Cached(controller.TokenReviews(c)), log)})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	authn.Register(mux, api.Base)
	mcp.NewBoard(m, log, &authz.Policy{}).Register(mux, authn)
	srv.Config.Handler = mux

	// status is what the board answers an initialize sent with tok.
	status := func(tok string) int {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
		req, _ := http.NewRequest(http.MethodPost, srv.URL+mcp.BoardPath, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	call := func(tok, tool string, args map[string]any) *sdk.CallToolResult {
		t.Helper()
		s, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil).Connect(ctx, &sdk.StreamableClientTransport{
			Endpoint: srv.URL + mcp.BoardPath, HTTPClient: &http.Client{Transport: bearer(tok)}, MaxRetries: -1,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		res, err := s.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	text := func(res *sdk.CallToolResult) string {
		b, _ := json.Marshal(res.Content)
		return string(b)
	}
	authors := func() []string {
		t.Helper()
		thread, err := m.ReadThread(ctx, storage.Key{Namespace: "team-a", Name: "x"}, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range thread {
			out = append(out, string(e.Author.Kind)+" "+e.Author.Name)
		}
		return out
	}

	// In the cluster: ci-bot's ServiceAccount creates a task and comments,
	// as AgentAccess ci-bot, a member.
	ci := saToken("team-a", "ci-bot")
	if res := call(ci, "create_task", map[string]any{"namespace": "team-a", "title": "from ci"}); res.IsError {
		t.Fatalf("create_task: %s", text(res))
	}
	if res := call(ci, "comment", map[string]any{"namespace": "team-a", "task": "x", "body": "from ci"}); res.IsError {
		t.Fatalf("comment: %s", text(res))
	}
	// Elsewhere it has no role, as a stranger has none.
	if res := call(ci, "create_task", map[string]any{"namespace": "team-b", "title": "elsewhere"}); !res.IsError || !strings.Contains(text(res), "you have no role there") {
		t.Fatalf("create_task in team-b: %s", text(res))
	}

	// Outside it: bot's minted token, read as an outside agent would.
	tok := minted("")
	if res := call(tok, "comment", map[string]any{"namespace": "team-a", "task": "x", "body": "from bot"}); res.IsError {
		t.Fatalf("comment with the minted token: %s", text(res))
	}
	if got := authors(); len(got) != 2 || got[0] != "User agent:team-a/ci-bot" || got[1] != "User agent:team-a/bot" {
		t.Fatalf("authors %q", got)
	}

	// A Runner's ServiceAccount, and cadre-runner's, are no agents.
	for name, tok := range map[string]string{"a Runner's": saToken("team-a", "runner"), "cadre-runner's": saToken("cadre-system", "cadre-runner")} {
		if got := status(tok); got != http.StatusUnauthorized {
			t.Errorf("%s ServiceAccount: %d", name, got)
		}
	}

	// Deleting the Secret rotates the token: the old one stops, the new
	// one works.
	if err := c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot.access-token"}}); err != nil {
		t.Fatal(err)
	}
	next := minted(tok)
	for deadline := time.Now().Add(10 * time.Second); status(tok) != http.StatusUnauthorized; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the token of a deleted Secret still works")
		}
	}
	for deadline := time.Now().Add(10 * time.Second); status(next) != http.StatusOK; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the new token does not work")
		}
	}
	// Deleting the AgentAccess revokes it at once: its token is read
	// fresh, never cached.
	if err := c.Delete(ctx, &v1alpha1.AgentAccess{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bot"}}); err != nil {
		t.Fatal(err)
	}
	if got := status(next); got != http.StatusUnauthorized {
		t.Fatalf("the token of a deleted AgentAccess: %d", got)
	}
}
