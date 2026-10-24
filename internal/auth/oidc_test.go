package auth_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/auth/oidctest"
)

const base = "/api/v1"

// server is a cadre-server stand-in: the auth routes, one API route that
// says who it saw, and a page to land on.
type server struct {
	*httptest.Server
	idp   *oidctest.Provider
	authn *auth.OIDC
	clock *clock
}

type clock struct {
	mu sync.Mutex
	d  time.Duration
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.d)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.d += d
}

func start(t *testing.T) *server {
	t.Helper()
	s := &server{idp: oidctest.New(t), clock: &clock{}}
	mux := http.NewServeMux()
	s.Server = httptest.NewServer(nil)
	t.Cleanup(s.Close)
	authn, err := auth.NewOIDC(context.Background(), auth.Config{
		Issuer: s.idp.URL, ClientID: oidctest.ClientID, ClientSecret: oidctest.ClientSecret, CLIClientID: oidctest.CLIClientID,
		Scopes: []string{"openid", "email", "profile", "groups"}, UsernameClaim: "email", GroupsClaim: "groups",
		PublicURL: s.URL, SessionKey: []byte(strings.Repeat("k", 32)), Now: s.clock.now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.authn = authn
	authn.Register(mux, base)
	mux.HandleFunc(base+"/whoami", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, auth.Author(r, r.URL.Query().Get("author")))
	})
	mux.HandleFunc("GET /t/", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "board") })
	s.Config.Handler = authn.Wrap(mux, base)
	return s
}

// browser follows redirects and keeps cookies, as a browser does.
func browser(t *testing.T) *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func (s *server) login(t *testing.T, c *http.Client, next string) *http.Response {
	t.Helper()
	res, err := c.Get(s.URL + "/auth/login?next=" + url.QueryEscape(next))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func body(t *testing.T, res *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func get(t *testing.T, c *http.Client, url string, header http.Header) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	return res, body(t, res)
}

func TestLogin(t *testing.T) {
	s := start(t)
	c := browser(t)
	res := s.login(t, c, "/t/team-a")
	if res.Request.URL.Path != "/t/team-a" || body(t, res) != "board" {
		t.Fatalf("landed on %s", res.Request.URL)
	}
	res, me := get(t, c, s.URL+base+"/me", nil)
	var got auth.Me
	if err := json.Unmarshal([]byte(me), &got); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("/me: %d %s", res.StatusCode, me)
	}
	if got.Mode != auth.ModeOIDC || got.Identity == nil || got.Username != "dana@example.test" || got.Name != "Dana Example" || strings.Join(got.Groups, ",") != "backend,oncall" {
		t.Errorf("/me: %+v", got)
	}
	// The session cookie is not for scripts, and goes with top-level
	// navigation only.
	u, _ := url.Parse(s.URL)
	var session *http.Cookie
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == "cadre-session" {
			session = ck
		}
	}
	if session == nil {
		t.Fatal("no session cookie")
	}
	login := s.login(t, browser(t), "/")
	for _, h := range login.Request.Response.Header.Values("Set-Cookie") {
		if strings.HasPrefix(h, "cadre-session=") && (!strings.Contains(h, "HttpOnly") || !strings.Contains(h, "SameSite=Lax") || !strings.Contains(h, "Path=/")) {
			t.Errorf("session cookie %q", h)
		}
	}
}

func TestSessionIsCappedAndExpires(t *testing.T) {
	s := start(t)
	s.idp.TTL = 48 * time.Hour
	c := browser(t)
	s.login(t, c, "/t/x")
	if res, _ := get(t, c, s.URL+base+"/whoami", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("logged in: %d", res.StatusCode)
	}
	s.clock.advance(auth.MaxSession + time.Minute)
	if res, _ := get(t, c, s.URL+base+"/whoami", nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after %s: %d, want 401", auth.MaxSession, res.StatusCode)
	}
}

func TestCallbackRefuses(t *testing.T) {
	t.Run("bad state", func(t *testing.T) {
		s := start(t)
		c := browser(t)
		c.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
			if req.URL.Path == "/auth/callback" {
				q := req.URL.Query()
				q.Set("state", "forged")
				req.URL.RawQuery = q.Encode()
			}
			return nil
		}
		if res := s.login(t, c, "/t/x"); res.StatusCode != http.StatusUnauthorized || !strings.Contains(body(t, res), "state mismatch") {
			t.Fatalf("forged state: %d", res.StatusCode)
		}
	})
	t.Run("bad nonce", func(t *testing.T) {
		s := start(t)
		s.idp.SetNonce("replayed")
		if res := s.login(t, browser(t), "/t/x"); res.StatusCode != http.StatusUnauthorized || !strings.Contains(body(t, res), "nonce mismatch") {
			t.Fatalf("wrong nonce: %d", res.StatusCode)
		}
	})
	t.Run("no login in progress", func(t *testing.T) {
		s := start(t)
		if res, _ := get(t, browser(t), s.URL+"/auth/callback?code=x&state=y", nil); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("callback alone: %d", res.StatusCode)
		}
	})
	t.Run("no username", func(t *testing.T) {
		s := start(t)
		s.idp.SetClaim("email", "")
		if res := s.login(t, browser(t), "/t/x"); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("no email claim: %d", res.StatusCode)
		}
	})
}

func TestNextStaysOnThisSite(t *testing.T) {
	for _, next := range []string{"https://evil.example.test/", "//evil.example.test/x", "/\\evil.example.test", "javascript:alert(1)", ""} {
		if got := auth.SafeNext(next); got != "/" {
			t.Errorf("SafeNext(%q) = %q, want /", next, got)
		}
	}
	for _, next := range []string{"/", "/t/a", "/t/a/tasks/x?tab=runs"} {
		if got := auth.SafeNext(next); got != next {
			t.Errorf("SafeNext(%q) = %q", next, got)
		}
	}
	s := start(t)
	c := browser(t)
	c.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if req.URL.Host == "evil.example.test" {
			t.Errorf("redirected off site to %s", req.URL)
			return http.ErrUseLastResponse
		}
		return nil
	}
	if res := s.login(t, c, "https://evil.example.test/"); res.Request.URL.Path != "/" {
		t.Errorf("landed on %s", res.Request.URL)
	}
}

func TestBearer(t *testing.T) {
	s := start(t)
	c := &http.Client{}
	bearer := func(tok string) http.Header { return http.Header{"Authorization": {"Bearer " + tok}} }
	for name, tc := range map[string]struct {
		token string
		code  int
	}{
		"the CLI's client":     {s.idp.IDToken(oidctest.CLIClientID, "", 0), http.StatusOK},
		"the browser's client": {s.idp.IDToken(oidctest.ClientID, "", 0), http.StatusOK},
		"another audience":     {s.idp.IDToken("someone-else", "", 0), http.StatusUnauthorized},
		"expired":              {s.idp.IDToken(oidctest.CLIClientID, "", -time.Minute), http.StatusUnauthorized},
		"tampered":             {s.idp.IDToken(oidctest.CLIClientID, "", 0) + "x", http.StatusUnauthorized},
		"not a JWT":            {"opaque", http.StatusUnauthorized},
	} {
		res, out := get(t, c, s.URL+base+"/whoami?author=mallory", bearer(tc.token))
		if res.StatusCode != tc.code {
			t.Errorf("%s: %d %s, want %d", name, res.StatusCode, out, tc.code)
		}
		// The author is the token's user, not what the client said.
		if tc.code == http.StatusOK && out != "dana@example.test" {
			t.Errorf("%s: author %q", name, out)
		}
	}
}

func TestIdentityRequired(t *testing.T) {
	s := start(t)
	c := &http.Client{}
	res, out := get(t, c, s.URL+base+"/whoami", nil)
	if res.StatusCode != http.StatusUnauthorized || !strings.Contains(out, "unauthenticated") {
		t.Errorf("no identity: %d %s", res.StatusCode, out)
	}
	res, out = get(t, c, s.URL+base+"/auth", nil)
	var info auth.Info
	if err := json.Unmarshal([]byte(out), &info); err != nil || res.StatusCode != http.StatusOK || info.Mode != auth.ModeOIDC || info.Issuer != s.idp.URL || info.CLIClientID != oidctest.CLIClientID {
		t.Errorf("/auth: %d %s", res.StatusCode, out)
	}
	// Outside the API nothing is checked: the UI's pages, the login.
	if res, _ := get(t, c, s.URL+"/t/x", nil); res.StatusCode != http.StatusOK {
		t.Errorf("/t/x: %d", res.StatusCode)
	}
}

func TestCSRF(t *testing.T) {
	s := start(t)
	c := browser(t)
	s.login(t, c, "/t/x")
	post := func(header http.Header) int {
		req, _ := http.NewRequest(http.MethodPost, s.URL+base+"/whoami", strings.NewReader("{}"))
		for k, v := range header {
			req.Header[k] = v
		}
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	for name, tc := range map[string]struct {
		header http.Header
		code   int
	}{
		"same origin":       {http.Header{"Origin": {s.URL}}, http.StatusOK},
		"same referer":      {http.Header{"Referer": {s.URL + "/t/x"}}, http.StatusOK},
		"another origin":    {http.Header{"Origin": {"https://evil.example.test"}}, http.StatusForbidden},
		"a lookalike":       {http.Header{"Referer": {s.URL + ".evil.example.test/"}}, http.StatusForbidden},
		"no origin":         {nil, http.StatusForbidden},
		"bearer, no origin": {http.Header{"Authorization": {"Bearer " + s.idp.IDToken(oidctest.CLIClientID, "", 0)}}, http.StatusOK},
	} {
		if got := post(tc.header); got != tc.code {
			t.Errorf("%s: %d, want %d", name, got, tc.code)
		}
	}
}

func TestLogout(t *testing.T) {
	s := start(t)
	c := browser(t)
	s.login(t, c, "/t/x")
	logout := func(origin string) int {
		req, _ := http.NewRequest(http.MethodPost, s.URL+"/auth/logout", nil)
		req.Header.Set("Origin", origin)
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	if code := logout("https://evil.example.test"); code != http.StatusForbidden {
		t.Errorf("logout from another site: %d", code)
	}
	if res, _ := get(t, c, s.URL+base+"/whoami", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("still logged in after a refused logout: %d", res.StatusCode)
	}
	if code := logout(s.URL); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	if res, _ := get(t, c, s.URL+base+"/whoami", nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("after logout: %d, want 401", res.StatusCode)
	}
}

func TestSessionKey(t *testing.T) {
	hexKey := strings.Repeat("ab", 32)
	if k, err := auth.ParseSessionKey([]byte(hexKey + "\n")); err != nil || len(k) != 32 || k[0] != 0xab {
		t.Errorf("hex key: %x %v", k, err)
	}
	if k, err := auth.ParseSessionKey([]byte(strings.Repeat("r", 32))); err != nil || len(k) != 32 {
		t.Errorf("raw key: %v", err)
	}
	for _, bad := range []string{"short", strings.Repeat("r", 31), strings.Repeat("z", 64)} {
		if _, err := auth.ParseSessionKey([]byte(bad)); err == nil {
			t.Errorf("key %q accepted", bad)
		}
	}
}

// Behind https the cookies are Secure and __Host-: sent only to this host,
// over https.
func TestCookiesBehindHTTPS(t *testing.T) {
	idp := oidctest.New(t)
	authn, err := auth.NewOIDC(context.Background(), auth.Config{
		Issuer: idp.URL, ClientID: oidctest.ClientID, UsernameClaim: "email",
		PublicURL: "https://cadre.example.test", SessionKey: []byte(strings.Repeat("k", 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	authn.Register(mux, base)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/login?next=/t/a", nil))
	set := w.Header().Get("Set-Cookie")
	if !strings.HasPrefix(set, "__Host-cadre-login=") || !strings.Contains(set, "Secure") || !strings.Contains(set, "HttpOnly") {
		t.Errorf("login cookie %q", set)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if loc.Query().Get("redirect_uri") != "https://cadre.example.test/auth/callback" || loc.Query().Get("code_challenge_method") != "S256" || loc.Query().Get("nonce") == "" {
		t.Errorf("authorization URL %s", loc)
	}
}
