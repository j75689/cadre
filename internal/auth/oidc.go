package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/j75689/cadre/internal/httpjson"
)

// MaxSession bounds a session, whatever the ID token's own expiry: there is
// no refresh, so a session past it logs in again.
const MaxSession = 12 * time.Hour

// loginTTL bounds the time between starting a login and its callback.
const loginTTL = 10 * time.Minute

// Config configures OIDC authentication.
type Config struct {
	// Issuer is the OIDC provider's issuer URL; its discovery document is
	// read at start.
	Issuer string
	// ClientID and ClientSecret are the browser's client; CLIClientID, by
	// default ClientID, is the one cadre login uses, a public client
	// without a secret. ID tokens for either are accepted.
	ClientID, ClientSecret, CLIClientID string
	Scopes                              []string
	// OfflineAccess lets cadre login ask for offline_access, so a person
	// gets a refresh token where the provider offers one.
	OfflineAccess bool
	// UsernameClaim names a user; GroupsClaim lists their groups.
	UsernameClaim, GroupsClaim string
	// PublicURL is where browsers reach cadre-server: the callback is
	// <PublicURL>/auth/callback, cookies are Secure when it is https, and
	// a cookie-authenticated change must come from its origin.
	PublicURL string
	// SessionKey (32 bytes) encrypts the cookies, the same on every
	// replica.
	SessionKey []byte
	// Agents, when set, authenticates the bearer tokens of agents outside
	// Cadre, beside ID tokens.
	Agents Agents
	// HTTPClient reaches the provider; Now is the clock. For tests.
	HTTPClient *http.Client
	Now        func() time.Time
	Log        *slog.Logger
}

// maxCookie bounds the sealed session: browsers drop a cookie past about
// 4 KB without a word, which would send the user round the login forever.
const maxCookie = 3800

// JustLoggedIn names a short-lived cookie the callback sets beside the
// session, readable by the page: a page that finds it and is still not
// logged in knows the session cookie was dropped, and does not send the
// browser round the login again. It is neither Secure nor __Host-, so that
// it arrives over plain http too, where a Secure session would not.
const JustLoggedIn = "cadre-just-logged-in"

// discoveryRetry bounds the wait between failed discoveries.
const discoveryRetry = 30 * time.Second

// OIDC is --auth=oidc.
type OIDC struct {
	cfg Config
	// ready holds the provider once its discovery document is read: until
	// then nobody can log in or be verified, but sessions still work.
	ready   atomic.Pointer[discovered]
	seal    sealer
	origin  string // of PublicURL, as originOf gives it
	secure  bool
	session string // cookie names
	login   string
}

type discovered struct {
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
}

// NewOIDC returns the authenticator and reads the provider's discovery
// document: once now, and if that fails again in the background until it
// succeeds or ctx ends. A provider that is down does not stop cadre-server
// starting: logins and bearer tokens are answered 503 meanwhile.
func NewOIDC(ctx context.Context, cfg Config) (*OIDC, error) {
	switch {
	case cfg.Issuer == "" || cfg.ClientID == "":
		return nil, errors.New("OIDC needs an issuer and a client id")
	case cfg.UsernameClaim == "":
		return nil, errors.New("OIDC needs a username claim")
	}
	if cfg.CLIClientID == "" {
		cfg.CLIClientID = cfg.ClientID
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	pub, err := url.Parse(cfg.PublicURL)
	switch {
	case err != nil || (pub.Scheme != "http" && pub.Scheme != "https") || pub.Host == "":
		return nil, fmt.Errorf("external URL %q: not an http or https URL", cfg.PublicURL)
	case (pub.Path != "" && pub.Path != "/") || pub.RawQuery != "" || pub.Fragment != "":
		// /auth/* and the UI's paths are at the root.
		return nil, fmt.Errorf("external URL %q: must be an origin (scheme, host and port), with no path", cfg.PublicURL)
	}
	seal, err := newSealer(cfg.SessionKey)
	if err != nil {
		return nil, err
	}
	origin, _ := originOf(cfg.PublicURL)
	o := &OIDC{cfg: cfg, seal: seal, origin: origin, secure: pub.Scheme == "https", session: "cadre-session", login: "cadre-login"}
	if o.secure {
		// __Host- cookies are sent only to this host, over https, for /.
		o.session, o.login = "__Host-"+o.session, "__Host-"+o.login
	}
	if err := o.discover(ctx); err != nil {
		// A wrong issuer stays wrong: say so now. A provider that is
		// down or slow may come back.
		if !transient(err) {
			return nil, fmt.Errorf("OIDC discovery for %s: %w", cfg.Issuer, err)
		}
		cfg.Log.Warn("OIDC discovery failed; logins wait for it, retrying", "issuer", cfg.Issuer, "err", err)
		go o.rediscover(ctx)
	}
	return o, nil
}

// transient reports whether a discovery error may go away by itself: the
// network, a timeout, or the provider answering 5xx. An issuer mismatch, a
// 4xx or a malformed document will not.
func transient(err error) bool {
	var netErr net.Error
	var urlErr *url.Error
	if errors.As(err, &netErr) || errors.As(err, &urlErr) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// go-oidc reports a status it did not want as "<code> <text>: <body>".
	msg := err.Error()
	return len(msg) >= 3 && msg[0] == '5' && msg[1] >= '0' && msg[1] <= '9' && msg[2] >= '0' && msg[2] <= '9'
}

// discover reads the discovery document, within a bound.
func (o *OIDC) discover(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if o.cfg.HTTPClient != nil {
		ctx = oidc.ClientContext(ctx, o.cfg.HTTPClient)
	}
	provider, err := oidc.NewProvider(ctx, o.cfg.Issuer)
	if err != nil {
		return err
	}
	o.ready.Store(&discovered{
		// The audience is checked by hand: either client's tokens are
		// accepted.
		verifier: provider.Verifier(&oidc.Config{SkipClientIDCheck: true, Now: o.cfg.Now}),
		oauth: oauth2.Config{
			ClientID: o.cfg.ClientID, ClientSecret: o.cfg.ClientSecret, Endpoint: provider.Endpoint(),
			RedirectURL: strings.TrimRight(o.cfg.PublicURL, "/") + "/auth/callback", Scopes: o.cfg.Scopes,
		},
	})
	return nil
}

// rediscover retries discovery with a growing pause until it succeeds.
func (o *OIDC) rediscover(ctx context.Context) {
	for wait := time.Second; ; wait = min(2*wait, discoveryRetry) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		err := o.discover(ctx)
		switch {
		case err == nil:
			o.cfg.Log.Info("OIDC discovery succeeded; logins are open", "issuer", o.cfg.Issuer)
			return
		case !transient(err):
			o.cfg.Log.Error("OIDC discovery failed for good: logins stay closed until cadre-server restarts with a working --oidc-issuer", "issuer", o.cfg.Issuer, "err", err)
			return
		}
		o.cfg.Log.Warn("OIDC discovery failed again", "issuer", o.cfg.Issuer, "err", err)
	}
}

// Ready reports whether the provider has been discovered.
func (o *OIDC) Ready() bool { return o.ready.Load() != nil }

const notReady = "logins are not ready: cadre-server cannot reach the OIDC provider yet"

// Info implements Authenticator.
func (o *OIDC) Info() Info {
	return Info{Mode: ModeOIDC, Issuer: o.cfg.Issuer, ClientID: o.cfg.ClientID, CLIClientID: o.cfg.CLIClientID, Scopes: o.cfg.Scopes, OfflineAccess: o.cfg.OfflineAccess}
}

// session is what the session cookie holds.
type session struct {
	Identity
	Expires time.Time `json:"exp"`
}

// pending is what the login cookie holds between login and callback.
type pending struct {
	State, Nonce, Verifier, Next string
	Expires                      time.Time
}

// Register implements Authenticator.
func (o *OIDC) Register(mux *http.ServeMux, base string) {
	mux.HandleFunc("GET /auth/login", o.startLogin)
	mux.HandleFunc("GET /auth/callback", o.callback)
	mux.HandleFunc("POST /auth/logout", o.logout)
	mux.HandleFunc("GET "+base+"/auth", func(w http.ResponseWriter, _ *http.Request) { httpjson.Write(w, http.StatusOK, o.Info()) })
}

// SafeNext returns next, cleaned, if it is a path on this server, and "/"
// otherwise: a login must not end on another site, nor on a login. The path
// is cleaned here as http.Redirect would clean it, so what is checked is
// what the browser gets: "/./\evil.example" would otherwise become
// "/\evil.example", which browsers read as another host. A backslash is
// refused outright.
func SafeNext(next string) string {
	if len(next) > maxNext || !strings.HasPrefix(next, "/") || strings.ContainsAny(next, "\\\x00\r\n\t") {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	// The escaped path, so an encoded "/" in a segment stays one.
	escaped := u.EscapedPath()
	clean := path.Clean(escaped)
	if strings.HasSuffix(escaped, "/") && clean != "/" {
		clean += "/"
	}
	if clean == "/auth" || strings.HasPrefix(clean, "/auth/") {
		return "/"
	}
	if u.RawQuery != "" {
		clean += "?" + u.RawQuery
	}
	return clean
}

// maxNext bounds next, which the login cookie carries: a cookie too large
// for the browser would be dropped and the login fail.
const maxNext = 2000

func random() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (o *OIDC) cookie(name, value string, maxAge time.Duration) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: o.secure, SameSite: http.SameSiteLaxMode, MaxAge: int(maxAge / time.Second)}
}

func (o *OIDC) clear(name string) *http.Cookie {
	c := o.cookie(name, "", 0)
	c.MaxAge = -1
	return c
}

func (o *OIDC) startLogin(w http.ResponseWriter, r *http.Request) {
	d := o.ready.Load()
	if d == nil {
		http.Error(w, notReady, http.StatusServiceUnavailable)
		return
	}
	// Behind https the cookies are Secure: reached over plain http, the
	// browser would drop them and the login could never finish.
	if o.secure && r.TLS == nil && !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		http.Error(w, "This server expects to be reached over https, at "+o.cfg.PublicURL+": log in from there.", http.StatusBadRequest)
		return
	}
	p := pending{State: random(), Nonce: random(), Verifier: oauth2.GenerateVerifier(), Next: SafeNext(r.URL.Query().Get("next")), Expires: o.cfg.Now().Add(loginTTL)}
	sealed, err := o.seal.seal(o.login, p)
	if err != nil {
		http.Error(w, "could not start a login", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, o.cookie(o.login, sealed, loginTTL))
	http.Redirect(w, r, d.oauth.AuthCodeURL(p.State, oidc.Nonce(p.Nonce), oauth2.S256ChallengeOption(p.Verifier)), http.StatusFound)
}

// loginFailed ends a callback that went wrong. The details go to the log:
// they are no business of the browser's.
func (o *OIDC) loginFailed(w http.ResponseWriter, why string, err error) {
	o.cfg.Log.Warn("OIDC login failed", "why", why, "err", err)
	http.SetCookie(w, o.clear(o.login))
	http.Error(w, "login failed: "+why+"; start again at /auth/login", http.StatusUnauthorized)
}

func (o *OIDC) callback(w http.ResponseWriter, r *http.Request) {
	d := o.ready.Load()
	if d == nil {
		http.Error(w, notReady, http.StatusServiceUnavailable)
		return
	}
	var p pending
	c, err := r.Cookie(o.login)
	if err == nil {
		err = o.seal.open(o.login, c.Value, &p)
	}
	q := r.URL.Query()
	switch {
	case err != nil:
		o.loginFailed(w, "no login in progress", err)
		return
	case o.cfg.Now().After(p.Expires):
		o.loginFailed(w, "the login took too long", nil)
		return
	case q.Get("error") != "":
		o.loginFailed(w, "the provider refused: "+q.Get("error"), errors.New(q.Get("error_description")))
		return
	case q.Get("state") == "" || q.Get("state") != p.State:
		o.loginFailed(w, "state mismatch", nil)
		return
	}
	ctx := r.Context()
	if o.cfg.HTTPClient != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, o.cfg.HTTPClient)
	}
	tok, err := d.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(p.Verifier))
	if err != nil {
		o.loginFailed(w, "the code exchange failed", err)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		o.loginFailed(w, "no ID token", nil)
		return
	}
	id, expires, nonce, err := o.verify(ctx, d, raw)
	if err != nil {
		o.loginFailed(w, "the ID token is not valid", err)
		return
	}
	if nonce != p.Nonce {
		o.loginFailed(w, "nonce mismatch", nil)
		return
	}
	if limit := o.cfg.Now().Add(MaxSession); expires.After(limit) {
		expires = limit
	}
	sealed, err := o.seal.seal(o.session, session{Identity: *id, Expires: expires})
	if err != nil {
		o.loginFailed(w, "the session could not be made", err)
		return
	}
	if len(o.session)+len(sealed) > maxCookie {
		o.loginFailed(w, fmt.Sprintf("the session is too large for a browser cookie (%d groups in the %q claim); "+
			"have the provider send fewer, for example only the groups Cadre's roles name", len(id.Groups), o.cfg.GroupsClaim), nil)
		return
	}
	http.SetCookie(w, o.clear(o.login))
	http.SetCookie(w, o.cookie(o.session, sealed, expires.Sub(o.cfg.Now())))
	marker := o.cookie(JustLoggedIn, "1", time.Minute)
	marker.HttpOnly, marker.Secure = false, false
	http.SetCookie(w, marker)
	http.Redirect(w, r, p.Next, http.StatusFound)
}

func (o *OIDC) logout(w http.ResponseWriter, r *http.Request) {
	if !o.sameOrigin(r) {
		httpjson.WriteError(w, http.StatusForbidden, httpjson.CodeForbidden, "a logout must come from this site")
		return
	}
	http.SetCookie(w, o.clear(o.session))
	// The marker too, or a 401 within its minute would read as a dropped
	// session rather than a logout.
	marker := o.clear(JustLoggedIn)
	marker.HttpOnly, marker.Secure = false, false
	http.SetCookie(w, marker)
	w.WriteHeader(http.StatusNoContent)
}

// verify checks an ID token: signature, issuer, expiry, and an audience of
// one of the two clients. It returns the identity, the token's expiry and
// its nonce.
func (o *OIDC) verify(ctx context.Context, d *discovered, raw string) (*Identity, time.Time, string, error) {
	if o.cfg.HTTPClient != nil {
		ctx = oidc.ClientContext(ctx, o.cfg.HTTPClient)
	}
	tok, err := d.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, time.Time{}, "", err
	}
	if !slices.Contains(tok.Audience, o.cfg.ClientID) && !slices.Contains(tok.Audience, o.cfg.CLIClientID) {
		return nil, time.Time{}, "", fmt.Errorf("audience %v is not this server's client", tok.Audience)
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return nil, time.Time{}, "", err
	}
	username, _ := claims[o.cfg.UsernameClaim].(string)
	if username == "" {
		return nil, time.Time{}, "", fmt.Errorf("the token has no %q claim", o.cfg.UsernameClaim)
	}
	// An address is matched without case: Dana@Example.test is
	// dana@example.test.
	if o.cfg.UsernameClaim == "email" {
		username = strings.ToLower(username)
	}
	// agent:<namespace>/<name> is an agent's: no login may pass for one.
	if strings.HasPrefix(strings.ToLower(username), AgentPrefix) {
		return nil, time.Time{}, "", fmt.Errorf("the username %q is reserved for agents", username)
	}
	// An address the provider has not verified names nobody, when the
	// address is the username: anyone could have registered it. Without
	// the claim the provider vouches for it.
	if v, ok := claims["email_verified"]; o.cfg.UsernameClaim == "email" && ok && (v == false || v == "false") {
		return nil, time.Time{}, "", errors.New("the token's email is not verified")
	}
	name, _ := claims["name"].(string)
	if name == "" {
		name = username
	}
	id := &Identity{Subject: tok.Subject, Username: username, Name: name, Groups: groups(claims[o.cfg.GroupsClaim])}
	return id, tok.Expiry, tok.Nonce, nil
}

// groups reads a groups claim: a list of strings, or one string.
func groups(v any) []string {
	switch g := v.(type) {
	case string:
		return []string{g}
	case []any:
		var out []string
		for _, x := range g {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// sameOrigin reports whether a browser request came from this site: its
// Origin, or failing that its Referer, is the public URL's.
func (o *OIDC) sameOrigin(r *http.Request) bool {
	from := r.Header.Get("Origin")
	if from == "" {
		from = r.Header.Get("Referer")
	}
	got, ok := originOf(from)
	return ok && got == o.origin
}

// originOf is the scheme, host and port of a URL, the host in lower case
// and a default port dropped, so that equal origins compare equal.
func originOf(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return u.Scheme + "://" + host, true
}

// Bearer authenticates every request to next with a bearer ID token, and
// with nothing else: no session cookie, so no request from a page in a
// browser can act for its user. It is for endpoints called by programs,
// /mcp. The identity is built as Wrap builds it from a bearer token.
func (o *OIDC) Bearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.serveBearer(w, r, next)
	})
}

// serveBearer authenticates the request's bearer token and serves next
// with its identity, the one way both Wrap and Bearer read a bearer token.
// The token's shape routes it, before anything is checked: a JWT issued by
// the OIDC provider is an ID token, verified as such and nothing else; any
// other JWT is a ServiceAccount's, and a two-part token one minted for an
// AgentAccess, both for Agents. So no ID token ever reaches the API server
// in a TokenReview. This assumes the cluster's ServiceAccount issuer is
// not the OIDC issuer: if it were, ServiceAccount tokens would be verified
// as ID tokens, and refused. Anything else is refused unread.
func (o *OIDC) serveBearer(w http.ResponseWriter, r *http.Request, next http.Handler) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	tok := strings.TrimSpace(raw)
	var id *Identity
	var err error
	switch kind := o.tokenKind(tok); {
	case !ok || kind == "":
		err = ErrUnauthenticated
	case kind == kindIDToken:
		d := o.ready.Load()
		if d == nil {
			httpjson.WriteError(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable, notReady)
			return
		}
		if id, _, _, err = o.verify(r.Context(), d, tok); err != nil {
			o.cfg.Log.Debug("a bearer ID token was refused", "err", err)
			err = ErrUnauthenticated
		}
	case o.cfg.Agents == nil:
		err = ErrUnauthenticated
	default:
		id, err = o.cfg.Agents.Authenticate(r.Context(), tok)
	}
	switch {
	case errors.Is(err, ErrUnauthenticated):
		httpjson.WriteError(w, http.StatusUnauthorized, httpjson.CodeUnauthenticated, msgBadBearer)
		return
	case err != nil:
		o.cfg.Log.Warn("authenticate an agent", "err", err)
		httpjson.WriteError(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable, "temporarily unavailable; retry")
		return
	}
	next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
}

// msgBadBearer answers every bearer token refused, whatever it is, so a
// caller learns nothing about which check failed.
const msgBadBearer = "the bearer token is not valid for this server: send an ID token (run cadre login) or an AgentAccess's token"

// Bearer token kinds.
const (
	kindIDToken = "id"    // a JWT the OIDC provider issued
	kindAgent   = "agent" // another JWT, or a minted token
)

// tokenKind says what tok is by its shape, unverified: an ID token when it
// is a JWT whose issuer is the OIDC provider, an agent's when it is
// another JWT or a two-part minted token, and "" otherwise.
func (o *OIDC) tokenKind(tok string) string {
	switch strings.Count(tok, ".") {
	case 1:
		return kindAgent
	case 2:
	default:
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if sameIssuer(o.cfg.Issuer, claims.Issuer) {
		return kindIDToken
	}
	return kindAgent
}

// googleIssuer is the one issuer go-oidc's verifier also accepts in
// another form: Google's ID tokens may say accounts.google.com, without
// the scheme.
const googleIssuer = "https://accounts.google.com"

// sameIssuer reports whether a token's iss is the configured issuer's, as
// the verifier will decide it: so a token it would verify as an ID token
// is always routed to it.
func sameIssuer(configured, iss string) bool {
	return iss == configured || configured == googleIssuer && iss == "accounts.google.com"
}

// Wrap implements Authenticator. Under base, every route but base/auth
// needs an identity: a bearer ID token, or the session cookie. A change
// made with the cookie must come from this site.
func (o *OIDC) Wrap(next http.Handler, base string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, base+"/") || r.URL.Path == base+"/auth" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "" {
			o.serveBearer(w, r, next)
			return
		}
		var s session
		c, err := r.Cookie(o.session)
		if err == nil {
			err = o.seal.open(o.session, c.Value, &s)
		}
		if err != nil || !o.cfg.Now().Before(s.Expires) {
			httpjson.WriteError(w, http.StatusUnauthorized, httpjson.CodeUnauthenticated, "log in at /auth/login")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !o.sameOrigin(r) {
			httpjson.WriteError(w, http.StatusForbidden, httpjson.CodeForbidden, "a change made with a session cookie must come from this site")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), &s.Identity)))
	})
}
