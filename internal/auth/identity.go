// Package auth authenticates the humans who use cadre-server's API and web
// UI: an OIDC login in the browser, held in a session cookie, or an ID token
// sent as a bearer token by the CLI. Runners and the per-run MCP endpoint
// authenticate their own way and are not touched here.
package auth

import (
	"context"
	"net/http"
)

// Identity is who a request comes from, as the OIDC provider said.
type Identity struct {
	Subject  string   `json:"subject"`
	Username string   `json:"username"`
	Name     string   `json:"name"`
	Groups   []string `json:"groups,omitempty"`
}

type identityKey struct{}

// WithIdentity returns ctx carrying id.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// FromContext returns the identity a request was authenticated as, if any.
func FromContext(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(*Identity)
	return id, ok && id != nil
}

// Author is the name to record as the author of what r does: the
// authenticated username, or, without authentication (--auth=none), the
// name the client gave.
func Author(r *http.Request, given string) string {
	if id, ok := FromContext(r.Context()); ok {
		return id.Username
	}
	return given
}

// Modes.
const (
	ModeOIDC = "oidc"
	ModeNone = "none"
)

// Info is what GET /api/v1/auth tells a client about how to log in.
type Info struct {
	Mode   string `json:"mode"`
	Issuer string `json:"issuer,omitempty"`
	// ClientID is the client the browser logs in with; CLIClientID the one
	// the CLI does, which may be the same.
	ClientID    string   `json:"clientId,omitempty"`
	CLIClientID string   `json:"cliClientId,omitempty"`
	Scopes      []string `json:"scopes,omitempty"`
}

// Me is GET /api/v1/me: the mode, and with OIDC the caller's identity, their
// role on each team they may see, by namespace, and whether they are a
// bootstrap admin.
type Me struct {
	Mode string `json:"mode"`
	*Identity
	Roles map[string]string `json:"roles,omitempty"`
	Admin bool              `json:"admin,omitempty"`
}

// Authenticator authenticates requests in one mode.
type Authenticator interface {
	// Info describes the mode to clients.
	Info() Info
	// Wrap authenticates the requests under the API's base path that next
	// would serve, and leaves every other path alone.
	Wrap(next http.Handler, base string) http.Handler
	// Bearer authenticates every request to next with a bearer token only,
	// for endpoints called by programs; without logins it checks nothing.
	Bearer(next http.Handler) http.Handler
	// Register adds the login routes, if the mode has any, and GET
	// <base>/auth. The API serves <base>/me.
	Register(mux *http.ServeMux, base string)
}
