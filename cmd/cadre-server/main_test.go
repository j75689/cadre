package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/auth/oidctest"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage/memory"
)

func TestValidateTimings(t *testing.T) {
	for _, tc := range []struct {
		lease, heartbeat time.Duration
		ok               bool
	}{
		{30 * time.Second, 10 * time.Second, true},
		{2 * time.Second, time.Second, true},
		{30 * time.Second, 500 * time.Millisecond, false},
		{1500 * time.Millisecond, time.Second, false},
		{10 * time.Second, 60 * time.Second, false},
		{10 * time.Second, 10 * time.Second, false},
		{0, 0, false},
	} {
		if err := validateTimings(tc.lease, tc.heartbeat); (err == nil) != tc.ok {
			t.Errorf("lease %s heartbeat %s: %v, want ok=%v", tc.lease, tc.heartbeat, err, tc.ok)
		}
	}
}

func TestBoundRequests(t *testing.T) {
	var deadline time.Time
	h := boundRequests(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		deadline, _ = r.Context().Deadline()
	}), time.Minute, 2*time.Minute)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	if until := time.Until(deadline); until <= 0 || until > time.Minute {
		t.Fatalf("request deadline in %s, want within a minute", until)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp/run", nil))
	if until := time.Until(deadline); until <= time.Minute || until > 2*time.Minute {
		t.Fatalf("MCP request deadline in %s, want the longer bound", until)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/namespaces/team-a/tasks/fix-login/approve", nil))
	if until := time.Until(deadline); until <= time.Minute || until > 2*time.Minute {
		t.Fatalf("approval deadline in %s, want the longer bound", until)
	}
}

func TestResolvePublicURL(t *testing.T) {
	for _, tc := range []struct {
		flag, listen string
		inCluster    bool
		want         string
		ok           bool
	}{
		{"", ":8080", false, "http://localhost:8080", true},
		{"", "[::]:9090", false, "http://localhost:9090", true},
		{"", "0.0.0.0:80", false, "http://localhost:80", true},
		{"https://cadre.example.com", ":8080", true, "https://cadre.example.com", true},
		{"", ":8080", true, "", false},
		{"", "8080", false, "", false},
	} {
		got, err := resolvePublicURL(tc.flag, tc.listen, tc.inCluster)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("resolvePublicURL(%q, %q, %v) = %q, %v; want %q, ok=%v", tc.flag, tc.listen, tc.inCluster, got, err, tc.want, tc.ok)
		}
	}
}

// Every route cadre-server serves registers on one mux without a conflict
// (ServeMux panics on overlapping patterns), and the mux keeps its 405s.
func TestRoutesCoexist(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := manager.New(memory.NewBackend().Open(), manager.Config{})
	mux := newMux(m, log, "http://localhost", auth.None{}, nil, nil)
	for _, c := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/api/v1/namespaces/a/tasks/x/cancel", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/v1/namespaces/a/agents", http.StatusMethodNotAllowed},
		{http.MethodGet, "/runner/v1/claim", http.StatusMethodNotAllowed},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/nope", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusOK},
		{http.MethodGet, "/t/a/tasks/x", http.StatusOK},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != c.code {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, w.Code, c.code)
		}
	}
}

func TestNewAuthenticator(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	if a, err := newAuthenticator(ctx, "", false, oidcFlags{}, "http://localhost:8080", log); err != nil || a.Info().Mode != auth.ModeNone {
		t.Errorf("outside a cluster: %v, %v", a, err)
	}
	// In a cluster logins are the default, and need their flags.
	if _, err := newAuthenticator(ctx, "", true, oidcFlags{}, "https://cadre.example.test", log); err == nil || !strings.Contains(err.Error(), "--oidc-issuer") {
		t.Errorf("in a cluster without OIDC flags: %v", err)
	}
	if _, err := newAuthenticator(ctx, "basic", false, oidcFlags{}, "http://localhost:8080", log); err == nil {
		t.Error("--auth=basic accepted")
	}
	idp := oidctest.New(t)
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte(strings.Repeat("ab", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := oidcFlags{issuer: idp.URL, clientID: oidctest.ClientID, scopes: "openid, email groups", usernameClaim: "email", groupsClaim: "groups", sessionKeyFile: key}
	a, err := newAuthenticator(ctx, "oidc", false, f, "http://localhost:8080", log)
	if err != nil || a.Info().Mode != auth.ModeOIDC || strings.Join(a.Info().Scopes, " ") != "openid email groups" {
		t.Fatalf("oidc: %+v, %v", a, err)
	}
	// Every route still registers beside the login's, and the API wants
	// an identity while the UI and the probes do not.
	mux := newMux(manager.New(memory.NewBackend().Open(), manager.Config{}), log, "http://localhost:8080", a, mustPolicy(t, a), nil)
	for _, c := range []struct {
		path string
		code int
	}{{"/api/v1/teams", http.StatusUnauthorized}, {"/api/v1/auth", http.StatusOK}, {"/healthz", http.StatusOK}, {"/t/a", http.StatusOK}, {"/auth/login", http.StatusFound}} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, c.path, nil))
		if w.Code != c.code {
			t.Errorf("GET %s: %d, want %d", c.path, w.Code, c.code)
		}
	}
}

func mustPolicy(t *testing.T, a auth.Authenticator) *authz.Policy {
	t.Helper()
	p, err := newPolicy(a, "", "", "email", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewPolicy(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	idp := oidctest.New(t)
	key := filepath.Join(t.TempDir(), "key")
	_ = os.WriteFile(key, []byte(strings.Repeat("ab", 32)), 0o600)
	oidcAuth, err := newAuthenticator(context.Background(), "oidc", false, oidcFlags{issuer: idp.URL, clientID: oidctest.ClientID, usernameClaim: "email", sessionKeyFile: key}, "http://localhost:8080", log)
	if err != nil {
		t.Fatal(err)
	}
	// Comma-separated only: a group's name may hold spaces.
	p, err := newPolicy(oidcAuth, " root@example.test , ", "Platform Team,ops", "email", log)
	if err != nil || strings.Join(p.AdminUsers, "|") != "root@example.test" || strings.Join(p.AdminGroups, "|") != "Platform Team|ops" || !p.FoldUsernames {
		t.Errorf("policy %+v, %v", p, err)
	}
	if p, err := newPolicy(auth.None{}, "", "", "email", log); p != nil || err != nil {
		t.Errorf("without logins: %+v, %v", p, err)
	}
	if _, err := newPolicy(oidcAuth, "root@example.test lead@example.test", "", "email", log); err == nil || !strings.Contains(err.Error(), "commas") {
		t.Errorf("space-separated admins: %v", err)
	}
	if _, err := newPolicy(auth.None{}, "root@example.test", "", "email", log); err == nil || !strings.Contains(err.Error(), "--auth=oidc") {
		t.Errorf("admins without logins: %v", err)
	}
}
