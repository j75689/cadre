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

// refreshEnv is a server with logins against idp, its user logged in with
// cadre login.
func refreshEnv(t *testing.T, idp *oidctest.Provider) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
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
func expire(t *testing.T, server string) credential {
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
	return c
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
	for name, refuse := range map[string]func(*oidctest.Provider){
		"revoked":      func(idp *oidctest.Provider) { idp.RevokeAll() },
		"another user": func(idp *oidctest.Provider) { idp.SetClaim("sub", "user-2") },
	} {
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
			if _, err := run("--server", server, "-n", "team-a", "task", "list"); err == nil || !strings.Contains(err.Error(), "expired; run cadre login") {
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

// Another cadre refreshing the same login first, the provider rotating the
// refresh token: the one that loses takes the login the other kept.
func TestRefreshRace(t *testing.T) {
	idp := oidctest.New(t)
	idp.SetOffline(true)
	server := refreshEnv(t, idp)
	stale := expire(t, server)
	orig := beforeRefresh
	t.Cleanup(func() { beforeRefresh = orig })
	beforeRefresh = func() {
		beforeRefresh = func() {}
		// The other cadre's refresh: a new login kept, the old refresh
		// token used up.
		other, err := refresh(context.Background(), stale)
		if err != nil {
			t.Errorf("the other refresh: %v", err)
			return
		}
		creds, _ := loadCredentials()
		creds[serverKey(server)] = other
		if err := creds.save(); err != nil {
			t.Error(err)
		}
	}
	out, err := run("--server", server, "auth", "token")
	if k := kept(t, server); err != nil || strings.TrimSpace(out) != k.IDToken || k.RefreshToken == "" || k.RefreshToken == stale.RefreshToken ||
		!k.Expires.After(time.Now().Add(time.Minute)) || idp.Refreshes() != 1 {
		t.Fatalf("the race: %q, %v; kept %+v", out, err, k)
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
