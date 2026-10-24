// Package oidctest is an OIDC provider for tests: discovery, a JWKS with a
// key generated per provider, an authorization endpoint that approves at
// once, and a token endpoint that checks the client and PKCE.
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
	Nonce string
	codes map[string]grant
}

type grant struct {
	client, redirect, nonce, challenge string
}

// New starts a provider, stopped when t ends. Its user is a fake one.
func New(t *testing.T) *Provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{
		key:    key,
		TTL:    time.Hour,
		codes:  map[string]grant{},
		Claims: map[string]any{"sub": "user-1", "email": "dana@example.test", "name": "Dana Example", "groups": []string{"backend", "oncall"}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /keys", p.keys)
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p.URL = srv.URL
	return p
}

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer": p.URL, "authorization_endpoint": p.URL + "/authorize", "token_endpoint": p.URL + "/token",
		"jwks_uri": p.URL + "/keys", "response_types_supported": []string{"code"},
		"subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"},
	})
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
	p.codes[code] = grant{client: client, redirect: q.Get("redirect_uri"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
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
		writeJSON(w, map[string]any{"access_token": "opaque", "token_type": "Bearer", "expires_in": 3600, "id_token": p.IDToken(client, nonce, 0)})
	}
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
