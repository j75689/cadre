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

// errRefreshRefused means the provider will not refresh this login: its
// refresh token is no good, or what came back is not this login's. Only
// a new login helps.
var errRefreshRefused = errors.New("refresh refused")

// currentLogin is the login kept for server, refreshed first when its ID
// token expires within tokenMargin and it has a refresh token. ok is false
// when there is none. A refresh the provider refuses drops the refresh
// token, and the login is returned as it was, expired; one that could not
// be made at all is an error, and the refresh token is kept for next time.
func currentLogin(ctx context.Context, server string) (credential, bool, error) {
	creds, err := loadCredentials()
	if err != nil {
		return credential{}, false, err
	}
	key := serverKey(server)
	cur, ok := creds[key]
	if !ok || cur.RefreshToken == "" || time.Now().Add(tokenMargin).Before(cur.Expires) {
		return cur, ok, nil
	}
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	beforeRefresh()
	next, err := refresh(ctx, cur)
	// Another cadre may have refreshed it meanwhile, using the refresh
	// token up if the provider rotates them: what it kept wins.
	if latest, lerr := loadCredentials(); lerr == nil {
		if l, ok := latest[key]; ok && !sameLogin(l, cur) && time.Now().Add(tokenMargin).Before(l.Expires) {
			return l, true, nil
		}
		creds = latest
	}
	switch {
	case errors.Is(err, errRefreshRefused):
		cur.RefreshToken = ""
		creds[key] = cur
		return cur, true, creds.save()
	case err != nil:
		return cur, true, fmt.Errorf("refresh the login to %s: %w", server, err)
	}
	creds[key] = next
	return next, true, creds.save()
}

// sameLogin reports whether a and b are the same kept login: an ID token
// signed again in the same second may read the same, so its expiry and
// refresh token count too.
func sameLogin(a, b credential) bool {
	return a.IDToken == b.IDToken && a.RefreshToken == b.RefreshToken && a.Expires.Equal(b.Expires)
}

// refresh gets a new ID token for cur with its refresh token. The answer
// must carry an ID token for the same issuer, client and subject.
func refresh(ctx context.Context, cur credential) (credential, error) {
	provider, err := oidc.NewProvider(ctx, cur.Issuer)
	if err != nil {
		return credential{}, fmt.Errorf("OIDC discovery for %s: %w", cur.Issuer, err)
	}
	conf := oauth2.Config{ClientID: cur.ClientID, Endpoint: provider.Endpoint()}
	tok, err := conf.TokenSource(ctx, &oauth2.Token{RefreshToken: cur.RefreshToken}).Token()
	var refused *oauth2.RetrieveError
	switch {
	case errors.As(err, &refused) && refused.Response != nil && refused.Response.StatusCode < 500:
		return credential{}, fmt.Errorf("%w: %s", errRefreshRefused, refused.ErrorCode)
	case err != nil:
		return credential{}, err
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return credential{}, fmt.Errorf("%w: the answer has no ID token", errRefreshRefused)
	}
	id, err := provider.Verifier(&oidc.Config{ClientID: cur.ClientID}).Verify(ctx, raw)
	switch {
	case err != nil:
		return credential{}, fmt.Errorf("%w: its ID token: %w", errRefreshRefused, err)
	case id.Subject != cur.Subject:
		return credential{}, fmt.Errorf("%w: its ID token is another user's", errRefreshRefused)
	}
	next := cur
	next.IDToken, next.Expires = raw, id.Expiry
	if tok.RefreshToken != "" {
		next.RefreshToken = tok.RefreshToken // rotated
	}
	return next, nil
}

func newLogoutCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Forget the login kept for the server, revoking its refresh token where the provider can",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			creds, err := loadCredentials()
			if err != nil {
				return err
			}
			key := serverKey(g.server)
			cur, ok := creds[key]
			if !ok {
				outf(cmd.OutOrStdout(), "No login kept for %s.\n", g.server)
				return nil
			}
			// Forgotten whatever the provider says: a revocation that
			// fails must not keep the token here.
			delete(creds, key)
			if err := creds.save(); err != nil {
				return err
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
