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

// me answers who Wrap found, as the API's /me does.
func me(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(auth.Me{Mode: auth.ModeOIDC, Identity: id})
}

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
	mux.HandleFunc("GET "+base+"/me", me)
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
	u, _ := url.Parse(s.URL)
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == auth.JustLoggedIn || ck.Name == "cadre-session" {
			t.Errorf("cookie %s left after logout", ck.Name)
		}
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
	mux.HandleFunc("GET "+base+"/me", me)
	// Reached over plain http, the login would lose its Secure cookies:
	// refused with a pointer to the https URL.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/login?next=/t/a", nil))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "https://cadre.example.test") {
		t.Errorf("over http: %d %s", w.Code, w.Body.String())
	}
	// Behind a proxy that ends TLS.
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/login?next=/t/a", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	mux.ServeHTTP(w, req)
	set := w.Header().Get("Set-Cookie")
	if !strings.HasPrefix(set, "__Host-cadre-login=") || !strings.Contains(set, "Secure") || !strings.Contains(set, "HttpOnly") {
		t.Errorf("login cookie %q", set)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if loc.Query().Get("redirect_uri") != "https://cadre.example.test/auth/callback" || loc.Query().Get("code_challenge_method") != "S256" || loc.Query().Get("nonce") == "" {
		t.Errorf("authorization URL %s", loc)
	}
}

func TestSafeNextAfterCleaning(t *testing.T) {
	for next, want := range map[string]string{
		"/./\\evil.example.test":          "/",
		"/x/../\\evil.example.test":       "/",
		"/.//evil.example.test":           "/evil.example.test",
		"/auth/login":                     "/",
		"/auth/../auth/callback":          "/",
		"/auth":                           "/",
		"/authors":                        "/authors",
		"/a/./b/../c":                     "/a/c",
		"/t/a/":                           "/t/a/",
		"/t/a?x=1&y=%2F":                  "/t/a?x=1&y=%2F",
		"/t/a\r\nSet-Cookie: x":           "/",
		"/t/" + strings.Repeat("a", 2001): "/",
	} {
		if got := auth.SafeNext(next); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", next, got, want)
		}
	}
}

func TestOriginOf(t *testing.T) {
	for raw, want := range map[string]string{
		"https://cadre.example.test:443/x": "https://cadre.example.test",
		"HTTPS://Cadre.Example.Test":       "https://cadre.example.test",
		"http://localhost:80":              "http://localhost",
		"http://localhost:8080/t/a":        "http://localhost:8080",
		"https://[::1]:8443":               "https://[::1]:8443",
	} {
		if got, ok := auth.OriginOf(raw); !ok || got != want {
			t.Errorf("OriginOf(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
	for _, bad := range []string{"", "null", "javascript:x", "cadre.example.test"} {
		if got, ok := auth.OriginOf(bad); ok {
			t.Errorf("OriginOf(%q) = %q", bad, got)
		}
	}
}

// An unverified email counts only when the email is the username.
func TestEmailVerifiedForAnotherUsernameClaim(t *testing.T) {
	idp := oidctest.New(t)
	idp.SetClaim("preferred_username", "dana")
	idp.SetClaim("email_verified", false)
	authn, err := auth.NewOIDC(context.Background(), auth.Config{
		Issuer: idp.URL, ClientID: oidctest.ClientID, UsernameClaim: "preferred_username", PublicURL: "http://localhost:8080",
		SessionKey: []byte(strings.Repeat("k", 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	authn.Register(mux, base)
	mux.HandleFunc("GET "+base+"/me", me)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, base+"/me", nil)
	req.Header.Set("Authorization", "Bearer "+idp.IDToken(oidctest.ClientID, "", 0))
	authn.Wrap(mux, base).ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"username":"dana"`) {
		t.Errorf("preferred_username with an unverified email: %d %s", w.Code, w.Body.String())
	}
}

func TestEmailMustBeVerified(t *testing.T) {
	s := start(t)
	c := &http.Client{}
	for v, code := range map[any]int{false: http.StatusUnauthorized, "false": http.StatusUnauthorized, true: http.StatusOK} {
		s.idp.SetClaim("email_verified", v)
		if res, _ := get(t, c, s.URL+base+"/whoami", http.Header{"Authorization": {"Bearer " + s.idp.IDToken(oidctest.CLIClientID, "", 0)}}); res.StatusCode != code {
			t.Errorf("email_verified %v: %d, want %d", v, res.StatusCode, code)
		}
	}
}

// A session too large for a cookie is refused with a reason, rather than
// set and dropped by the browser; a good one comes with the marker the UI
// reads.
func TestSessionCookieSize(t *testing.T) {
	s := start(t)
	res := s.login(t, browser(t), "/t/x")
	var marker string
	for r := res; r != nil; r = r.Request.Response {
		for _, h := range r.Header.Values("Set-Cookie") {
			if strings.HasPrefix(h, auth.JustLoggedIn+"=") {
				marker = h
			}
		}
	}
	if marker == "" || strings.Contains(marker, "HttpOnly") || strings.Contains(marker, "Secure") {
		t.Errorf("just-logged-in cookie %q", marker)
	}
	many := make([]string, 300)
	for i := range many {
		many[i] = "team-with-a-rather-long-group-name-" + strings.Repeat("x", i%7)
	}
	s.idp.SetClaim("groups", many)
	res = s.login(t, browser(t), "/t/x")
	if out := body(t, res); res.StatusCode != http.StatusUnauthorized || !strings.Contains(out, "300 groups") {
		t.Fatalf("a huge session: %d %s", res.StatusCode, out)
	}
}

// A provider that is down at start does not stop the server: logins and
// bearer tokens wait for it, answered 503, and work once it is back.
func TestDiscoveryRetries(t *testing.T) {
	idp := oidctest.New(t)
	idp.SetDown(true)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	authn, err := auth.NewOIDC(ctx, auth.Config{
		Issuer: idp.URL, ClientID: oidctest.ClientID, UsernameClaim: "email", PublicURL: "http://localhost:8080",
		SessionKey: []byte(strings.Repeat("k", 32)), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil || authn.Ready() {
		t.Fatalf("with the provider down: %v, ready %v", err, authn.Ready())
	}
	mux := http.NewServeMux()
	authn.Register(mux, base)
	mux.HandleFunc("GET "+base+"/me", me)
	h := authn.Wrap(mux, base)
	serve := func(path string, header http.Header) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for k, v := range header {
			req.Header[k] = v
		}
		h.ServeHTTP(w, req)
		return w.Code
	}
	bearer := http.Header{"Authorization": {"Bearer " + idp.IDToken(oidctest.ClientID, "", 0)}}
	if code := serve("/auth/login", nil); code != http.StatusServiceUnavailable {
		t.Errorf("login while down: %d", code)
	}
	if code := serve(base+"/me", bearer); code != http.StatusServiceUnavailable {
		t.Errorf("bearer while down: %d", code)
	}
	if code := serve(base+"/auth", nil); code != http.StatusOK {
		t.Errorf("/auth while down: %d", code)
	}
	idp.SetDown(false)
	deadline := time.Now().Add(10 * time.Second)
	for !authn.Ready() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if code := serve(base+"/me", bearer); code != http.StatusOK {
		t.Errorf("bearer once back: %d", code)
	}
}

// A wrong issuer stays wrong: refused at start, not retried for ever.
func TestDiscoveryFailsFastWhenMisconfigured(t *testing.T) {
	idp := oidctest.New(t)
	cfg := func(issuer string) auth.Config {
		return auth.Config{Issuer: issuer, ClientID: oidctest.ClientID, UsernameClaim: "email", PublicURL: "http://localhost:8080",
			SessionKey: []byte(strings.Repeat("k", 32)), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	}
	if _, err := auth.NewOIDC(context.Background(), cfg(idp.URL+"/nope")); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("an issuer that is not one: %v", err)
	}
	idp.SetIssuer("https://someone-else.example.test")
	if _, err := auth.NewOIDC(context.Background(), cfg(idp.URL)); err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Errorf("an issuer mismatch: %v", err)
	}
	// The network failing is worth waiting out.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if a, err := auth.NewOIDC(ctx, cfg("http://127.0.0.1:1")); err != nil || a.Ready() {
		t.Errorf("an unreachable provider: %v", err)
	}
}

func TestExternalURLIsAnOrigin(t *testing.T) {
	idp := oidctest.New(t)
	for _, u := range []string{"https://cadre.example.test/cadre", "https://cadre.example.test/?x=1", "cadre.example.test"} {
		if _, err := auth.NewOIDC(context.Background(), auth.Config{Issuer: idp.URL, ClientID: oidctest.ClientID, UsernameClaim: "email",
			PublicURL: u, SessionKey: []byte(strings.Repeat("k", 32))}); err == nil {
			t.Errorf("external URL %q accepted", u)
		}
	}
	if _, err := auth.NewOIDC(context.Background(), auth.Config{Issuer: idp.URL, ClientID: oidctest.ClientID, UsernameClaim: "email",
		PublicURL: "https://cadre.example.test/", SessionKey: []byte(strings.Repeat("k", 32))}); err != nil {
		t.Errorf("an origin with a slash: %v", err)
	}
}
