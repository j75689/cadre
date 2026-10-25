// Package oidctest is an OIDC provider for tests: discovery, a JWKS with a
// key generated per provider, an authorization endpoint that approves at
// once, and a token endpoint that checks the client and PKCE. With
// SetOffline it also issues refresh tokens, rotated on every use, and with
// SetRevocation it has a revocation endpoint (RFC 7009).
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Clients the provider knows.
const (
	ClientID     = "cadre"
	ClientSecret = "test-client-secret"
	CLIClientID  = "cadre-cli" // public: no secret
)

// Provider is a running test provider.
type Provider struct {
	URL string
	key *rsa.PrivateKey

	mu sync.Mutex
	// Claims are the user's claims in the next ID tokens.
	Claims map[string]any
	// TTL is how long an ID token is valid.
	TTL time.Duration
	// Nonce, when set, replaces the nonce asked for, as a broken or
	// hostile provider would.
	Nonce  string
	down   bool
	issuer string // what discovery claims, if not URL
	codes  map[string]grant
	// offline: offline_access is offered, and a refresh token issued for
	// it; refresh: the live refresh tokens, by token, with their client;
	// revoke: a revocation endpoint is offered; revoked: what it revoked.
	offline, revoke bool
	refresh         map[string]string
	revoked         []string
	refreshes       int
	refreshStatus   int // when set, every refresh is answered with it
}

type grant struct {
	client, redirect, nonce, challenge string
	offline                            bool
}

// New starts a provider, stopped when t ends. Its user is a fake one.
func New(t *testing.T) *Provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{
		key:     key,
		TTL:     time.Hour,
		codes:   map[string]grant{},
		refresh: map[string]string{},
		Claims:  map[string]any{"sub": "user-1", "email": "dana@example.test", "name": "Dana Example", "groups": []string{"backend", "oncall"}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /keys", p.keys)
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	mux.HandleFunc("POST /revoke", p.revocation)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p.URL = srv.URL
	return p
}

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	down, issuer := p.down, p.issuer
	p.mu.Unlock()
	if down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	if issuer == "" {
		issuer = p.URL
	}
	doc := map[string]any{
		"issuer": issuer, "authorization_endpoint": p.URL + "/authorize", "token_endpoint": p.URL + "/token",
		"jwks_uri": p.URL + "/keys", "response_types_supported": []string{"code"},
		"subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"},
	}
	p.mu.Lock()
	if p.offline {
		doc["scopes_supported"] = []string{"openid", "email", "profile", "groups", "offline_access"}
	}
	if p.revoke {
		doc["revocation_endpoint"] = p.URL + "/revoke"
	}
	p.mu.Unlock()
	writeJSON(w, doc)
}

func (p *Provider) keys(w http.ResponseWriter, _ *http.Request) {
	pub := p.key.PublicKey
	writeJSON(w, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test",
		"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	client := q.Get("client_id")
	if (client != ClientID && client != CLIClientID) || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	code := b64([]byte(time.Now().String() + q.Get("state")))
	p.mu.Lock()
	p.codes[code] = grant{client: client, redirect: q.Get("redirect_uri"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge"),
		offline: p.offline && slices.Contains(strings.Fields(q.Get("scope")), "offline_access")}
	p.mu.Unlock()
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	v := back.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	back.RawQuery = v.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	client, secret, basic := r.BasicAuth()
	if !basic {
		client, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if r.PostForm.Get("grant_type") == "refresh_token" {
		p.refreshGrant(w, client, r.PostForm.Get("refresh_token"))
		return
	}
	p.mu.Lock()
	g, ok := p.codes[r.PostForm.Get("code")]
	delete(p.codes, r.PostForm.Get("code"))
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	switch {
	case !ok || g.client != client || g.redirect != r.PostForm.Get("redirect_uri"):
		tokenError(w, "invalid_grant")
	case client == ClientID && secret != ClientSecret:
		tokenError(w, "invalid_client")
	case b64(sum[:]) != g.challenge:
		tokenError(w, "invalid_grant") // PKCE
	default:
		nonce := g.nonce
		p.mu.Lock()
		if p.Nonce != "" {
			nonce = p.Nonce
		}
		p.mu.Unlock()
		out := map[string]any{"access_token": "opaque", "token_type": "Bearer", "expires_in": 3600, "id_token": p.IDToken(client, nonce, 0)}
		if g.offline {
			out["refresh_token"] = p.newRefresh(client)
		}
		writeJSON(w, out)
	}
}

// newRefresh issues a refresh token for client.
func (p *Provider) newRefresh(client string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := "refresh-" + b64(b)
	p.mu.Lock()
	p.refresh[tok] = client
	p.mu.Unlock()
	return tok
}

// refreshGrant answers grant_type=refresh_token: a new ID token, with the
// provider's claims as they are now, and the refresh token rotated, so the
// one used is refused from then on.
func (p *Provider) refreshGrant(w http.ResponseWriter, client, tok string) {
	p.mu.Lock()
	if status := p.refreshStatus; status != 0 {
		p.mu.Unlock()
		http.Error(w, http.StatusText(status), status)
		return
	}
	owner, ok := p.refresh[tok]
	delete(p.refresh, tok)
	if ok {
		p.refreshes++
	}
	p.mu.Unlock()
	if !ok || owner != client {
		tokenError(w, "invalid_grant")
		return
	}
	writeJSON(w, map[string]any{"access_token": "opaque", "token_type": "Bearer", "expires_in": 3600,
		"id_token": p.IDToken(client, "", 0), "refresh_token": p.newRefresh(client)})
}

// revocation is RFC 7009's endpoint: the token is revoked, known or not.
func (p *Provider) revocation(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.revoke {
		http.NotFound(w, r)
		return
	}
	tok := r.PostForm.Get("token")
	delete(p.refresh, tok)
	p.revoked = append(p.revoked, tok)
}

// SetOffline makes the provider offer offline_access, and issue a refresh
// token to a login that asks for it.
func (p *Provider) SetOffline(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.offline = on
}

// SetRevocation makes the provider offer a revocation endpoint.
func (p *Provider) SetRevocation(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.revoke = on
}

// Revoked is the tokens the revocation endpoint was given.
func (p *Provider) Revoked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.revoked...)
}

// Refreshes is how many refresh grants the provider answered.
func (p *Provider) Refreshes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refreshes
}

// SetRefreshStatus makes every refresh answered with status, as a provider
// throttling or behind a proxy would; 0 answers them again.
func (p *Provider) SetRefreshStatus(status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshStatus = status
}

// RevokeAll revokes every refresh token, as a provider ending a session
// would.
func (p *Provider) RevokeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refresh = map[string]string{}
}

// IDToken signs an ID token for audience, with the provider's claims and
// nonce if set, valid for ttl (the provider's TTL when 0; negative for one
// already expired).
func (p *Provider) IDToken(audience, nonce string, ttl time.Duration) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ttl == 0 {
		ttl = p.TTL
	}
	now := time.Now()
	claims := map[string]any{"iss": p.URL, "aud": audience, "iat": now.Unix(), "exp": now.Add(ttl).Unix()}
	for k, v := range p.Claims {
		claims[k] = v
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "test"})
	body, _ := json.Marshal(claims)
	signing := b64(header) + "." + b64(body)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return signing + "." + b64(sig)
}

// SetClaim sets one claim of the next ID tokens.
func (p *Provider) SetClaim(name string, v any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Claims[name] = v
}

// SetDown makes discovery fail, as a provider that is down, or work again.
func (p *Provider) SetDown(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
}

// SetIssuer makes discovery claim another issuer, as a misconfigured
// provider or a wrong --oidc-issuer would.
func (p *Provider) SetIssuer(issuer string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.issuer = issuer
}

// SetNonce makes the next ID tokens carry nonce, whatever was asked.
func (p *Provider) SetNonce(nonce string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Nonce = nonce
}

func tokenError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func b64(b []byte) string { return strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=") }
