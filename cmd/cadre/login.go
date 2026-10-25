package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"

	"github.com/j75689/cadre/internal/auth"
	"github.com/j75689/cadre/internal/client"
)

// credentials are the logins cadre login stored, by server URL: the ID
// token, and, where the provider issues one, a refresh token, a long-lived
// secret that gets the next ID token without a browser. Deleting the file,
// or cadre logout, forgets them; against a provider without a revocation
// endpoint that is all there is to revoking a refresh token.
type credentials map[string]credential

type credential struct {
	IDToken string    `json:"idToken"`
	Expires time.Time `json:"expires"`
	// RefreshToken, with the issuer and client it is for and the subject
	// it logged in, refreshes the ID token; empty where the provider
	// issued none, and then an expired ID token means logging in again.
	RefreshToken string `json:"refreshToken,omitempty"`
	Issuer       string `json:"issuer,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	Subject      string `json:"subject,omitempty"`
}

// credentialsPath is $XDG_CONFIG_HOME/cadre/credentials, by default under
// ~/.config.
func credentialsPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "cadre", "credentials"), nil
}

func loadCredentials() (credentials, error) {
	path, err := credentialsPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return credentials{}, nil
	}
	if err != nil {
		return nil, err
	}
	creds := credentials{}
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return creds, nil
}

func (c credentials) save() error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// Written whole to a file of its own, 0600 from the start, and renamed,
	// so a reader never sees half a file and two writers never share one.
	tmp, err := os.CreateTemp(filepath.Dir(path), "credentials.*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// withCredentials runs change on the credentials under their lock, and
// saves them if it says to.
func withCredentials(change func(credentials) (save bool, err error)) error {
	unlock, err := lockCredentials()
	if err != nil {
		return err
	}
	defer unlock()
	creds, err := loadCredentials()
	if err != nil {
		return err
	}
	save, err := change(creds)
	if err != nil || !save {
		return err
	}
	return creds.save()
}

// serverKey names a server in the credentials: its URL without a trailing
// slash.
func serverKey(server string) string { return strings.TrimRight(server, "/") }

// openBrowser shows url to the user; tests replace it.
var openBrowser = func(w io.Writer, url string) {
	outf(w, "Opening your browser to log in. If it does not open, visit:\n\n  %s\n\n", url)
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// loginTimeout bounds how long cadre login waits for the browser.
const loginTimeout = 5 * time.Minute

func newLoginCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Log in to the server with its OIDC provider",
		Long: "Log in to the server with its OIDC provider, in your browser. The ID token is kept in " +
			"$XDG_CONFIG_HOME/cadre/credentials (~/.config by default), readable by you only, and sent " +
			"with every request to this server until it expires. Where the provider offers offline_access, " +
			"a refresh token is kept there too, a long-lived secret that gets the next ID token without a " +
			"browser: cadre logout revokes it where the provider can, and deleting the file forgets it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), loginTimeout)
			defer cancel()
			info, err := client.New(g.server, nil).AuthInfo(ctx)
			if err != nil {
				return fmt.Errorf("ask %s how to log in: %w", g.server, err)
			}
			if info.Mode != auth.ModeOIDC {
				// A login kept from when it had them would only get in
				// the way.
				forgot := false
				if err := withCredentials(func(creds credentials) (bool, error) {
					_, forgot = creds[serverKey(g.server)]
					delete(creds, serverKey(g.server))
					return forgot, nil
				}); err != nil {
					return err
				}
				if forgot {
					outln(cmd.OutOrStdout(), "This server does not use logins (--auth="+info.Mode+"): forgot the login kept for it.")
					return nil
				}
				outln(cmd.OutOrStdout(), "This server does not use logins (--auth="+info.Mode+"): nothing to do.")
				return nil
			}
			tok, err := login(ctx, cmd.OutOrStdout(), info)
			if err != nil {
				return err
			}
			if err := withCredentials(func(creds credentials) (bool, error) {
				creds[serverKey(g.server)] = tok
				return true, nil
			}); err != nil {
				return err
			}
			me, err := g.client().Me(ctx)
			if err != nil {
				return fmt.Errorf("logged in, but the server refuses the token: %w", err)
			}
			if me.Identity == nil {
				return fmt.Errorf("logged in, but %s does not say who you are", g.server)
			}
			outf(cmd.OutOrStdout(), "Logged in to %s as %s.\n", g.server, me.Username)
			return nil
		},
	}
}

// login runs the authorization code flow with PKCE against the provider,
// with a callback on a localhost listener, and returns the ID token.
func login(ctx context.Context, w io.Writer, info *auth.Info) (credential, error) {
	provider, err := oidc.NewProvider(ctx, info.Issuer)
	if err != nil {
		return credential{}, fmt.Errorf("OIDC discovery for %s: %w", info.Issuer, err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return credential{}, err
	}
	defer func() { _ = ln.Close() }()
	clientID := info.CLIClientID
	if clientID == "" {
		clientID = info.ClientID
	}
	scopes := info.Scopes
	// A refresh token only where the server lets the CLI keep one, and the
	// provider says it gives one: asking another for offline_access may
	// fail the login.
	var offers struct {
		Scopes []string `json:"scopes_supported"`
	}
	if info.OfflineAccess && provider.Claims(&offers) == nil && slices.Contains(offers.Scopes, offlineAccess) && !slices.Contains(scopes, offlineAccess) {
		scopes = append(slices.Clone(scopes), offlineAccess)
	}
	conf := oauth2.Config{
		ClientID: clientID, Endpoint: provider.Endpoint(), Scopes: scopes,
		// The loopback address the listener is on, not "localhost", which
		// may resolve to another one (RFC 8252, 8.3); providers allow a
		// native client there at any port.
		RedirectURL: "http://" + ln.Addr().String() + "/callback",
	}
	state, nonce, verifier := randomString(), randomString(), oauth2.GenerateVerifier()
	type result struct {
		cred credential
		err  error
	}
	done := make(chan result, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// Only the provider's redirect, with this login's state, decides
		// it: anything else on the port is turned away and the wait goes
		// on.
		if r.URL.Path != "/callback" || r.URL.Query().Get("state") != state {
			http.Error(rw, "not this login's callback", http.StatusBadRequest)
			return
		}
		cred, err := finishLogin(r, &conf, provider.Verifier(&oidc.Config{ClientID: clientID}), nonce, verifier)
		cred.Issuer, cred.ClientID = info.Issuer, clientID
		if err != nil {
			http.Error(rw, "Login failed: "+err.Error(), http.StatusBadRequest)
		} else {
			_, _ = io.WriteString(rw, "Logged in to Cadre. You can close this tab.\n")
		}
		select {
		case done <- result{cred, err}:
		default:
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	// Shut down gracefully, so the page the browser waits for is sent.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	openBrowser(w, conf.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)))
	select {
	case r := <-done:
		return r.cred, r.err
	case <-ctx.Done():
		return credential{}, fmt.Errorf("no login within %s: %w", loginTimeout, ctx.Err())
	}
}

// finishLogin completes a callback whose state is this login's.
func finishLogin(r *http.Request, conf *oauth2.Config, verifier *oidc.IDTokenVerifier, nonce, codeVerifier string) (credential, error) {
	q := r.URL.Query()
	if q.Get("error") != "" {
		return credential{}, fmt.Errorf("the provider refused: %s %s", q.Get("error"), q.Get("error_description"))
	}
	tok, err := conf.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(codeVerifier))
	if err != nil {
		return credential{}, fmt.Errorf("code exchange: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	id, err := verifier.Verify(r.Context(), raw)
	if err != nil {
		return credential{}, fmt.Errorf("ID token: %w", err)
	}
	if id.Nonce != nonce {
		return credential{}, errors.New("nonce mismatch")
	}
	return credential{IDToken: raw, Expires: id.Expiry, RefreshToken: tok.RefreshToken, Subject: id.Subject}, nil
}

// offlineAccess is the scope that asks for a refresh token.
const offlineAccess = "offline_access"

func randomString() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
