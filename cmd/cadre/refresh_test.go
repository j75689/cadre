package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j75689/cadre/internal/api"
	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/auth/oidctest"
	"github.com/j75689/cadre/internal/authz"
	"github.com/j75689/cadre/internal/manager"
	"github.com/j75689/cadre/internal/storage/memory"
)

// refreshEnv is a server with logins against idp, letting the CLI keep a
// refresh token, its user logged in with cadre login.
func refreshEnv(t *testing.T, idp *oidctest.Provider) string {
	t.Helper()
	return refreshEnvWith(t, idp, true)
}

// refreshEnvWith is refreshEnv, the server saying offline whether the CLI
// may keep a refresh token.
func refreshEnvWith(t *testing.T, idp *oidctest.Provider, offline bool) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := manager.New(memory.NewBackend().Open(), manager.Config{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	authn, err := auth.NewOIDC(context.Background(), auth.Config{
		Issuer: idp.URL, ClientID: oidctest.ClientID, ClientSecret: oidctest.ClientSecret, CLIClientID: oidctest.CLIClientID,
		Scopes: []string{"openid", "email"}, UsernameClaim: "email", PublicURL: srv.URL, SessionKey: []byte(strings.Repeat("k", 32)), Log: log,
		OfflineAccess: offline,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	authn.Register(mux, api.Base)
	api.NewServer(m, log, api.WithAccess(auth.ModeOIDC, &authz.Policy{AdminUsers: []string{"dana@example.test"}})).Register(mux)
	srv.Config.Handler = authn.Wrap(mux, api.Base)
	orig := openBrowser
	openBrowser = func(_ io.Writer, authURL string) {
		go func() {
			if res, err := http.Get(authURL); err == nil {
				_ = res.Body.Close()
			}
		}()
	}
	t.Cleanup(func() { openBrowser = orig })
	if out, err := run("--server", srv.URL, "login"); err != nil {
		t.Fatalf("login: %q %v", out, err)
	}
	// From here on no command may open a browser.
	openBrowser = func(io.Writer, string) { t.Error("a browser was opened") }
	return srv.URL
}

// expire makes the kept ID token about to expire, as an hour later would.
func expire(t *testing.T, server string) {
	t.Helper()
	creds, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	c := creds[serverKey(server)]
	c.Expires = time.Now().Add(30 * time.Second)
	creds[serverKey(server)] = c
	if err := creds.save(); err != nil {
		t.Fatal(err)
	}
}

func kept(t *testing.T, server string) credential {
	t.Helper()
	creds, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	return creds[serverKey(server)]
}

// Where the provider offers offline_access, cadre login keeps a refresh
// token, and a command whose ID token is about to expire gets a new one
// with it, no browser opened; the provider rotates it, and the one used is
// refused from then on. cadre auth token refreshes too.
func TestLoginRefreshes(t *testing.T) {
	idp := oidctest.New(t)
	idp.SetOffline(true)
	server := refreshEnv(t, idp)
	first := kept(t, server)
	if first.RefreshToken == "" || first.Subject != "user-1" || first.Issuer != idp.URL || first.ClientID != oidctest.CLIClientID {
		t.Fatalf("the login kept %+v", first)
	}
	expire(t, server)
	if _, err := run("--server", server, "-n", "team-a", "team", "init"); err != nil {
		t.Fatalf("a command once the ID token is about to expire: %v", err)
	}
	second := kept(t, server)
	// The provider signs the same claims in the same second the same way,
	// so the ID token may read as the old one: its expiry and the rotated
	// refresh token say it is new.
	if idp.Refreshes() != 1 || second.RefreshToken == first.RefreshToken || !second.Expires.After(time.Now().Add(time.Minute)) {
		t.Fatalf("after a refresh: %d refreshes, kept %+v", idp.Refreshes(), second)
	}
	expire(t, server)
	out, err := run("--server", server, "auth", "token")
	if third := kept(t, server); err != nil || strings.TrimSpace(out) != third.IDToken || third.RefreshToken == second.RefreshToken || idp.Refreshes() != 2 {
		t.Fatalf("auth token: %q, %v, %d refreshes", out, err, idp.Refreshes())
	}
	// A login whose token is still good refreshes nothing.
	if _, err := run("--server", server, "-n", "team-a", "task", "list"); err != nil || idp.Refreshes() != 2 {
		t.Fatalf("a good token: %v, %d refreshes", err, idp.Refreshes())
	}
}

// A refresh the provider refuses, or one that comes back as another user,
// drops the refresh token: the login is then as one without, and expired.
func TestRefreshRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		refuse func(*oidctest.Provider)
		want   string // the reason the message gives
	}{
		"revoked":      {func(idp *oidctest.Provider) { idp.RevokeAll() }, "invalid_grant"},
		"another user": {func(idp *oidctest.Provider) { idp.SetClaim("sub", "user-2") }, "its ID token is another user's"},
	} {
		refuse, want := tc.refuse, tc.want
		t.Run(name, func(t *testing.T) {
			idp := oidctest.New(t)
			idp.SetOffline(true)
			server := refreshEnv(t, idp)
			refuse(idp)
			creds, _ := loadCredentials()
			c := creds[serverKey(server)]
			c.Expires = time.Now().Add(-time.Second)
			creds[serverKey(server)] = c
			if err := creds.save(); err != nil {
				t.Fatal(err)
			}
			if _, err := run("--server", server, "-n", "team-a", "task", "list"); err == nil || !strings.Contains(err.Error(), "the provider refused to refresh the login: "+want) || !strings.Contains(err.Error(), "run cadre login") {
				t.Fatalf("after a refused refresh: %v", err)
			}
			if k := kept(t, server); k.RefreshToken != "" || k.IDToken != c.IDToken {
				t.Fatalf("kept %+v: the refresh token should be gone, and nothing else changed", k)
			}
		})
	}
}

// Without offline_access no refresh token is asked for or kept, and an
// expired login says to log in again, as it always did.
func TestNoRefreshWithoutOffline(t *testing.T) {
	idp := oidctest.New(t)
	server := refreshEnv(t, idp)
	if k := kept(t, server); k.RefreshToken != "" {
		t.Fatalf("kept %+v", k)
	}
	expire(t, server)
	if _, err := run("--server", server, "auth", "token"); err == nil || !strings.Contains(err.Error(), "run cadre login") || idp.Refreshes() != 0 {
		t.Fatalf("auth token: %v, %d refreshes", err, idp.Refreshes())
	}
}

// A refresh the provider could not answer (throttling here) keeps the
// refresh token: a command goes on with the ID token while it lasts, and
// once it has expired says what failed, not to log in again.
func TestRefreshUnanswered(t *testing.T) {
	idp := oidctest.New(t)
	idp.SetOffline(true)
	server := refreshEnv(t, idp)
	tok := kept(t, server).RefreshToken
	idp.SetRefreshStatus(http.StatusTooManyRequests)
	expire(t, server) // 30 s left: within the margin, not expired
	if _, err := run("--server", server, "-n", "team-a", "task", "list"); err != nil {
		t.Fatalf("a command while the token lasts: %v", err)
	}
	if _, err := run("--server", server, "auth", "token"); err == nil || strings.Contains(err.Error(), "run cadre login") {
		t.Fatalf("auth token, which wants a minute: %v", err)
	}
	creds, _ := loadCredentials()
	c := creds[serverKey(server)]
	c.Expires = time.Now().Add(-time.Second)
	creds[serverKey(server)] = c
	if err := creds.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := run("--server", server, "-n", "team-a", "task", "list"); err == nil || strings.Contains(err.Error(), "run cadre login") || !strings.Contains(err.Error(), "refresh the login") {
		t.Fatalf("once expired: %v", err)
	}
	if k := kept(t, server); k.RefreshToken != tok {
		t.Fatalf("the refresh token was dropped: %+v", k)
	}
	// Once the provider answers again, it refreshes.
	idp.SetRefreshStatus(0)
	if _, err := run("--server", server, "-n", "team-a", "task", "list"); err != nil || idp.Refreshes() != 1 {
		t.Fatalf("after: %v, %d refreshes", err, idp.Refreshes())
	}
}

// Two cadres refreshing one login at once, the provider rotating the
// refresh token: the lock lets one refresh, and the other takes what it
// kept, so neither has to log in again.
func TestRefreshRace(t *testing.T) {
	idp := oidctest.New(t)
	idp.SetOffline(true)
	server := refreshEnv(t, idp)
	expire(t, server)
	orig := beforeRefresh
	t.Cleanup(func() { beforeRefresh = orig })
	beforeRefresh = func() { time.Sleep(200 * time.Millisecond) } // the other comes in meanwhile
	type result struct {
		c   credential
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			c, _, err := currentLogin(context.Background(), server)
			results <- result{c, err}
		}()
	}
	a, b := <-results, <-results
	k := kept(t, server)
	if a.err != nil || b.err != nil || idp.Refreshes() != 1 || k.RefreshToken == "" || a.c.RefreshToken != k.RefreshToken || b.c.RefreshToken != k.RefreshToken {
		t.Fatalf("%+v, %+v; %d refreshes; kept %+v", a, b, idp.Refreshes(), k)
	}
}

// A login logged out while a refresh waits for the lock stays logged out:
// the refresh finds it gone and writes nothing.
func TestLogoutDuringARefresh(t *testing.T) {
	idp := oidctest.New(t)
	idp.SetOffline(true)
	server := refreshEnv(t, idp)
	expire(t, server)
	unlock, err := lockCredentials()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() {
		_, ok, _ := currentLogin(context.Background(), server)
		done <- ok
	}()
	time.Sleep(100 * time.Millisecond) // it has read the login, and waits for the lock
	creds, _ := loadCredentials()
	delete(creds, serverKey(server))
	if err := creds.save(); err != nil {
		t.Fatal(err)
	}
	unlock()
	if ok := <-done; ok {
		t.Error("the refresh found a login logged out")
	}
	if creds, _ := loadCredentials(); len(creds) != 0 || idp.Refreshes() != 0 {
		t.Fatalf("after: %v, %d refreshes", creds, idp.Refreshes())
	}
}

// The server has the say: with --oidc-offline-access=false the CLI asks
// for no refresh token, even where the provider offers one.
func TestOfflineAccessOptOut(t *testing.T) {
	idp := oidctest.New(t)
	idp.SetOffline(true)
	server := refreshEnvWith(t, idp, false)
	if k := kept(t, server); k.RefreshToken != "" {
		t.Fatalf("kept %+v", k)
	}
}

// cadre logout forgets the login, and revokes its refresh token where the
// provider has a revocation endpoint; where it has none it says so.
func TestLogout(t *testing.T) {
	for _, revocation := range []bool{true, false} {
		idp := oidctest.New(t)
		idp.SetOffline(true)
		idp.SetRevocation(revocation)
		server := refreshEnv(t, idp)
		tok := kept(t, server).RefreshToken
		out, err := run("--server", server, "logout")
		if err != nil {
			t.Fatal(err)
		}
		if creds, _ := loadCredentials(); len(creds) != 0 {
			t.Errorf("revocation %v: still kept %v", revocation, creds)
		}
		switch got := idp.Revoked(); {
		case revocation && (len(got) != 1 || got[0] != tok || !strings.Contains(out, "refresh token revoked")):
			t.Errorf("revoked %v: %q", got, out)
		case !revocation && (len(got) != 0 || !strings.Contains(out, "no revocation endpoint")):
			t.Errorf("without an endpoint: revoked %v: %q", got, out)
		}
		if strings.Contains(out, tok) {
			t.Errorf("logout printed the refresh token: %q", out)
		}
	}
}
