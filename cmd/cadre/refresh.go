package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
)

// refreshTimeout bounds a refresh, discovery included.
const refreshTimeout = 30 * time.Second

// beforeRefresh runs between reading the login and refreshing it; tests
// replace it to race another cadre.
var beforeRefresh = func() {}

// refusedError means the provider will not refresh this login: its
// refresh token is no good (invalid_grant), or what came back is not this
// login's. Only a new login helps. Reason says why, never a token.
type refusedError struct{ reason string }

func (e *refusedError) Error() string {
	return "the provider refused to refresh the login: " + e.reason
}

// currentLogin is the login kept for server, refreshed first when its ID
// token expires within tokenMargin and it has a refresh token; ok is false
// when there is none. The refresh is made under the credentials' lock, on
// the login as it is then: another cadre may have refreshed it, or logged
// it out, meanwhile. A refresh the provider refuses drops the refresh
// token and is a *refusedError, with the login as it was; one that could
// not be made is another error, and the refresh token is kept for next
// time.
func currentLogin(ctx context.Context, server string) (credential, bool, error) {
	key := serverKey(server)
	creds, err := loadCredentials()
	if err != nil {
		return credential{}, false, err
	}
	if cur, ok := creds[key]; !ok || cur.RefreshToken == "" || goodFor(cur, tokenMargin) {
		return cur, ok, nil
	}
	var cur credential
	var ok bool
	var refreshErr error
	err = withCredentials(func(creds credentials) (bool, error) {
		if cur, ok = creds[key]; !ok || cur.RefreshToken == "" || goodFor(cur, tokenMargin) {
			return false, nil // logged out, or refreshed by another cadre
		}
		ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
		defer cancel()
		beforeRefresh()
		next, err := refresh(ctx, cur)
		var refused *refusedError
		switch {
		case errors.As(err, &refused):
			cur.RefreshToken = ""
			creds[key], refreshErr = cur, err
			return true, nil
		case err != nil:
			refreshErr = fmt.Errorf("refresh the login to %s: %w", server, err)
			return false, nil
		}
		creds[key], cur = next, next
		return true, nil
	})
	if err != nil {
		return cur, ok, err
	}
	return cur, ok, refreshErr
}

// goodFor reports whether c's ID token is still good for d.
func goodFor(c credential, d time.Duration) bool { return time.Now().Add(d).Before(c.Expires) }

// refresh gets a new ID token for cur with its refresh token. The answer
// must carry an ID token for the same issuer, client and subject.
func refresh(ctx context.Context, cur credential) (credential, error) {
	provider, err := oidc.NewProvider(ctx, cur.Issuer)
	if err != nil {
		return credential{}, fmt.Errorf("OIDC discovery for %s: %w", cur.Issuer, err)
	}
	conf := oauth2.Config{ClientID: cur.ClientID, Endpoint: provider.Endpoint()}
	tok, err := conf.TokenSource(ctx, &oauth2.Token{RefreshToken: cur.RefreshToken}).Token()
	var answer *oauth2.RetrieveError
	switch {
	case errors.As(err, &answer) && answer.ErrorCode == "invalid_grant":
		// RFC 6749's answer for a refresh token that is no good. Anything
		// else (throttling, a proxy, a client misconfigured) may pass.
		reason := answer.ErrorCode
		if answer.ErrorDescription != "" {
			reason += ": " + answer.ErrorDescription
		}
		return credential{}, &refusedError{reason}
	case err != nil:
		return credential{}, err
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return credential{}, &refusedError{"its answer has no ID token"}
	}
	id, err := provider.Verifier(&oidc.Config{ClientID: cur.ClientID}).Verify(ctx, raw)
	switch {
	case err != nil:
		return credential{}, &refusedError{"its ID token does not verify: " + err.Error()}
	case id.Subject != cur.Subject:
		return credential{}, &refusedError{"its ID token is another user's"}
	}
	next := cur
	// oauth2 keeps the old refresh token where the provider sent none.
	next.IDToken, next.Expires, next.RefreshToken = raw, id.Expiry, tok.RefreshToken
	return next, nil
}

func newLogoutCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Forget the login kept for the server, revoking its refresh token where the provider can",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			key := serverKey(g.server)
			var cur credential
			var ok bool
			// Forgotten whatever the provider says: a revocation that
			// fails must not keep the token here.
			if err := withCredentials(func(creds credentials) (bool, error) {
				cur, ok = creds[key]
				delete(creds, key)
				return ok, nil
			}); err != nil {
				return err
			}
			if !ok {
				outf(cmd.OutOrStdout(), "No login kept for %s.\n", g.server)
				return nil
			}
			out := cmd.OutOrStdout()
			if cur.RefreshToken == "" {
				outf(out, "Logged out of %s.\n", g.server)
				return nil
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), refreshTimeout)
			defer cancel()
			switch revoked, err := revoke(ctx, cur); {
			case err != nil:
				outf(out, "Logged out of %s; revoking its refresh token failed: %v\n", g.server, err)
			case !revoked:
				outf(out, "Logged out of %s; its refresh token is not revoked: the provider has no revocation endpoint.\n", g.server)
			default:
				outf(out, "Logged out of %s, and its refresh token revoked.\n", g.server)
			}
			return nil
		},
	}
}

// revoke revokes cur's refresh token at the provider's revocation endpoint
// (RFC 7009); revoked is false when the provider has none.
func revoke(ctx context.Context, cur credential) (revoked bool, err error) {
	provider, err := oidc.NewProvider(ctx, cur.Issuer)
	if err != nil {
		return false, fmt.Errorf("OIDC discovery for %s: %w", cur.Issuer, err)
	}
	var doc struct {
		Revocation string `json:"revocation_endpoint"`
	}
	if err := provider.Claims(&doc); err != nil || doc.Revocation == "" {
		return false, nil
	}
	form := url.Values{"token": {cur.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {cur.ClientID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, doc.Revocation, strings.NewReader(form.Encode()))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("the revocation endpoint answered %s", resp.Status)
	}
	return true, nil
}
