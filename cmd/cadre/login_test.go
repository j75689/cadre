package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/auth/oidctest"
	"github.com/j75689/cadre/internal/client"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/memory"
)

// run runs the CLI in-process with exactly args, and returns its output.
func run(args ...string) (string, error) {
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

// cadre login gets an ID token from the provider through the browser and a
// localhost callback, keeps it for the server only, and every command then
// acts as its user.
func TestLoginFromTheCLI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	idp := oidctest.New(t)
	m := manager.New(memory.NewBackend().Open(), manager.Config{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	authn, err := auth.NewOIDC(context.Background(), auth.Config{
		Issuer: idp.URL, ClientID: oidctest.ClientID, ClientSecret: oidctest.ClientSecret, CLIClientID: oidctest.CLIClientID,
		Scopes: []string{"openid", "email"}, UsernameClaim: "email", PublicURL: srv.URL, SessionKey: []byte(strings.Repeat("k", 32)), Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	authn.Register(mux, api.Base)
	api.NewServer(m, log).Register(mux)
	srv.Config.Handler = authn.Wrap(mux, api.Base)
	// The browser approves at once and follows the redirect home.
	opened := ""
	orig := openBrowser
	openBrowser = func(_ io.Writer, url string) {
		opened = url
		go func() {
			if res, err := http.Get(url); err == nil {
				_ = res.Body.Close()
			}
		}()
	}
	t.Cleanup(func() { openBrowser = orig })

	if _, err := run("--server", srv.URL, "-n", "team-a", "team", "init"); err == nil || !strings.Contains(err.Error(), "unauthenticated") {
		t.Fatalf("before login: %v", err)
	}
	out, err := run("--server", srv.URL, "login")
	if err != nil || !strings.Contains(out, "as dana@example.test") {
		t.Fatalf("login: %q %v", out, err)
	}
	if !strings.Contains(opened, "client_id="+oidctest.CLIClientID) || !strings.Contains(opened, "code_challenge=") {
		t.Errorf("authorization URL %s", opened)
	}
	path, _ := credentialsPath()
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file: %v, %v", info, err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "refresh") || !strings.Contains(string(data), "idToken") {
		t.Errorf("credentials hold %s", data)
	}

	if _, err := run("--server", srv.URL, "-n", "team-a", "team", "init"); err != nil {
		t.Fatalf("team init after login: %v", err)
	}
	if _, err := run("--server", srv.URL, "-n", "team-a", "task", "create", "Fix it"); err != nil {
		t.Fatal(err)
	}
	list, err := client.New(srv.URL, &http.Client{Transport: client.Bearer(idp.IDToken(oidctest.CLIClientID, "", 0), nil)}).ListTasks(context.Background(), "team-a", nil, 0, 0)
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("tasks: %+v %v", list, err)
	}
	name := list.Items[0].Name
	if _, err := run("--server", srv.URL, "-n", "team-a", "task", "comment", name, "-m", "hello"); err != nil {
		t.Fatal(err)
	}
	thread, err := m.ReadThread(context.Background(), storage.Key{Namespace: "team-a", Name: name}, 0, 0)
	if err != nil || len(thread) != 1 || thread[0].Author.Name != "dana@example.test" {
		t.Fatalf("thread: %+v %v", thread, err)
	}
	// Naming someone else is refused.
	if _, err := run("--server", srv.URL, "-n", "team-a", "--as", "mallory", "task", "comment", name, "-m", "hi"); err == nil || !strings.Contains(err.Error(), "identity comes from login") {
		t.Fatalf("--as with a login: %v", err)
	}
	// The token is this server's only.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("token sent to another server")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[]}`)
	}))
	t.Cleanup(other.Close)
	if _, err := run("--server", other.URL, "-n", "team-a", "task", "list"); err != nil {
		t.Fatal(err)
	}
}
