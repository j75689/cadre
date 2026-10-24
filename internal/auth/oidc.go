package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
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
	// UsernameClaim names a user; GroupsClaim lists their groups.
	UsernameClaim, GroupsClaim string
	// PublicURL is where browsers reach cadre-server: the callback is
	// <PublicURL>/auth/callback, cookies are Secure when it is https, and
	// a cookie-authenticated change must come from its origin.
	PublicURL string
	// SessionKey (32 bytes) encrypts the cookies, the same on every
	// replica.
	SessionKey []byte
	// HTTPClient reaches the provider; Now is the clock. For tests.
	HTTPClient *http.Client
	Now        func() time.Time
	Log        *slog.Logger
}

// OIDC is --auth=oidc.
type OIDC struct {
	cfg      Config
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
	seal     sealer
	origin   string // scheme://host of PublicURL
	secure   bool
	session  string // cookie names
	login    string
}

// NewOIDC reads the provider's discovery document and returns the
// authenticator.
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
	if err != nil || (pub.Scheme != "http" && pub.Scheme != "https") || pub.Host == "" {
		return nil, fmt.Errorf("public URL %q: not an http or https URL", cfg.PublicURL)
	}
	seal, err := newSealer(cfg.SessionKey)
	if err != nil {
		return nil, err
	}
	if cfg.HTTPClient != nil {
		ctx = oidc.ClientContext(ctx, cfg.HTTPClient)
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery for %s: %w", cfg.Issuer, err)
	}
	o := &OIDC{
		cfg: cfg,
		// The audience is checked by hand: either client's tokens are
		// accepted.
		verifier: provider.Verifier(&oidc.Config{SkipClientIDCheck: true, Now: cfg.Now}),
		oauth: oauth2.Config{
			ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, Endpoint: provider.Endpoint(),
			RedirectURL: strings.TrimRight(cfg.PublicURL, "/") + "/auth/callback", Scopes: cfg.Scopes,
		},
		seal:    seal,
		origin:  pub.Scheme + "://" + pub.Host,
		secure:  pub.Scheme == "https",
		session: "cadre-session",
		login:   "cadre-login",
	}
	if o.secure {
		// __Host- cookies are sent only to this host, over https, for /.
		o.session, o.login = "__Host-"+o.session, "__Host-"+o.login
	}
	return o, nil
}

// Info implements Authenticator.
func (o *OIDC) Info() Info {
	return Info{Mode: ModeOIDC, Issuer: o.cfg.Issuer, ClientID: o.cfg.ClientID, CLIClientID: o.cfg.CLIClientID, Scopes: o.cfg.Scopes}
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
	mux.HandleFunc("GET "+base+"/me", func(w http.ResponseWriter, r *http.Request) {
		id, _ := FromContext(r.Context()) // Wrap requires one
		httpjson.Write(w, http.StatusOK, Me{Mode: ModeOIDC, Identity: id})
	})
}

// SafeNext returns next if it is a path on this server, and "/" otherwise:
// a login must not end on another site.
func SafeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return next
}

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
	p := pending{State: random(), Nonce: random(), Verifier: oauth2.GenerateVerifier(), Next: SafeNext(r.URL.Query().Get("next")), Expires: o.cfg.Now().Add(loginTTL)}
	sealed, err := o.seal.seal(o.login, p)
	if err != nil {
		http.Error(w, "could not start a login", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, o.cookie(o.login, sealed, loginTTL))
	http.Redirect(w, r, o.oauth.AuthCodeURL(p.State, oidc.Nonce(p.Nonce), oauth2.S256ChallengeOption(p.Verifier)), http.StatusFound)
}

// loginFailed ends a callback that went wrong. The details go to the log:
// they are no business of the browser's.
func (o *OIDC) loginFailed(w http.ResponseWriter, why string, err error) {
	o.cfg.Log.Warn("OIDC login failed", "why", why, "err", err)
	http.SetCookie(w, o.clear(o.login))
	http.Error(w, "login failed: "+why+"; start again at /auth/login", http.StatusUnauthorized)
}

func (o *OIDC) callback(w http.ResponseWriter, r *http.Request) {
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
	tok, err := o.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(p.Verifier))
	if err != nil {
		o.loginFailed(w, "the code exchange failed", err)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		o.loginFailed(w, "no ID token", nil)
		return
	}
	id, expires, nonce, err := o.verify(ctx, raw)
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
	http.SetCookie(w, o.clear(o.login))
	http.SetCookie(w, o.cookie(o.session, sealed, expires.Sub(o.cfg.Now())))
	http.Redirect(w, r, p.Next, http.StatusFound)
}

func (o *OIDC) logout(w http.ResponseWriter, r *http.Request) {
	if !o.sameOrigin(r) {
		httpjson.WriteError(w, http.StatusForbidden, httpjson.CodeForbidden, "a logout must come from this site")
		return
	}
	http.SetCookie(w, o.clear(o.session))
	w.WriteHeader(http.StatusNoContent)
}

// verify checks an ID token: signature, issuer, expiry, and an audience of
// one of the two clients. It returns the identity, the token's expiry and
// its nonce.
func (o *OIDC) verify(ctx context.Context, raw string) (*Identity, time.Time, string, error) {
	if o.cfg.HTTPClient != nil {
		ctx = oidc.ClientContext(ctx, o.cfg.HTTPClient)
	}
	tok, err := o.verifier.Verify(ctx, raw)
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
	if origin := r.Header.Get("Origin"); origin != "" {
		return origin == o.origin
	}
	ref := r.Header.Get("Referer")
	return ref == o.origin || strings.HasPrefix(ref, o.origin+"/")
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
		if h := r.Header.Get("Authorization"); h != "" {
			raw, ok := strings.CutPrefix(h, "Bearer ")
			id, _, _, err := o.verify(r.Context(), strings.TrimSpace(raw))
			if !ok || err != nil {
				httpjson.WriteError(w, http.StatusUnauthorized, httpjson.CodeUnauthenticated, "the bearer token is not a valid ID token for this server; run cadre login")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
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
