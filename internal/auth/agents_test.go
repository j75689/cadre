package auth_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/auth/oidctest"
)

// fakeAgents knows one token, refuses the rest, and fails on "broken.x";
// it records every token it is asked about.
type fakeAgents struct {
	mu    sync.Mutex
	asked []string
	good  string
}

func (f *fakeAgents) Authenticate(_ context.Context, tok string) (*auth.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, tok)
	switch tok {
	case f.good:
		return auth.AgentIdentity("team-a", "bot", "member"), nil
	case "broken.x":
		return nil, errors.New("the API server is away")
	}
	return nil, auth.ErrUnauthenticated
}

func (f *fakeAgents) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// A bearer token is routed by its shape: a JWT from the OIDC provider is
// an ID token and nothing else; another JWT, or a two-part token, is an
// agent's; anything else is refused unread. An agent's refusal is a 401,
// and its failure to tell a 503.
func TestBearerRouting(t *testing.T) {
	agents := &fakeAgents{good: "dGVhbS1hL2JvdA.c2VjcmV0"}
	s := startWith(t, agents)
	call := func(tok string) (int, string) {
		t.Helper()
		res, b := get(t, http.DefaultClient, s.URL+base+"/whoami", http.Header{"Authorization": {"Bearer " + tok}})
		return res.StatusCode, b
	}

	// An ID token, still, and Agents never sees it.
	s.idp.SetClaim("email", "dana@example.test")
	if code, who := call(s.idp.IDToken(oidctest.ClientID, "", 0)); code != http.StatusOK || who != "dana@example.test" {
		t.Fatalf("an ID token: %d %q", code, who)
	}
	// One from the provider that does not verify is refused, and not
	// passed on: no ID token reaches a TokenReview.
	expired := s.idp.IDToken(oidctest.ClientID, "", -1)
	if code, _ := call(expired); code != http.StatusUnauthorized {
		t.Fatalf("an expired ID token: %d", code)
	}
	// An agent's minted token.
	if code, who := call(agents.good); code != http.StatusOK || who != "agent:team-a/bot" {
		t.Fatalf("a minted token: %d %q", code, who)
	}
	// A JWT from another issuer, a ServiceAccount's say, goes to Agents.
	s.idp.SetClaim("iss", "https://kubernetes.default.svc")
	sa := s.idp.IDToken("cadre", "", 0)
	if code, b := call(sa); code != http.StatusUnauthorized || !strings.Contains(b, "AgentAccess's token") {
		t.Fatalf("a refused ServiceAccount token: %d %q", code, b)
	}
	if code, _ := call("broken.x"); code != http.StatusServiceUnavailable {
		t.Fatalf("an agent's token that could not be checked: %d", code)
	}
	// Anything else is refused without being asked about.
	for _, bad := range []string{"not-a-token", "a.b.c.d", "a.%%%.c"} {
		if code, _ := call(bad); code != http.StatusUnauthorized {
			t.Errorf("%q: %d", bad, code)
		}
	}
	if got := agents.seen(); len(got) != 3 || got[0] != agents.good || got[1] != sa || got[2] != "broken.x" {
		t.Errorf("Agents was asked about %q", got)
	}
}

// Without Agents, an agent's token is refused like any other.
func TestBearerWithoutAgents(t *testing.T) {
	s := start(t)
	res, _ := get(t, http.DefaultClient, s.URL+base+"/whoami", http.Header{"Authorization": {"Bearer dGVhbS1hL2JvdA.c2VjcmV0"}})
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", res.StatusCode)
	}
}

// No login can pass for an agent: a username starting agent: is refused,
// whatever its case.
func TestAgentUsernameRefused(t *testing.T) {
	s := start(t)
	for _, user := range []string{"agent:team-a/bot", "Agent:team-a/bot"} {
		s.idp.SetClaim("email", user)
		res, _ := get(t, http.DefaultClient, s.URL+base+"/whoami", http.Header{"Authorization": {"Bearer " + s.idp.IDToken(oidctest.ClientID, "", 0)}})
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d", user, res.StatusCode)
		}
	}
}
