package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/j75689/cadre/internal/runner"
)

// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create

// RunnerAudience is the audience an in-cluster runner's ServiceAccount
// token must be issued for. A token for the API server's own audience is
// refused, so a token leaked from Cadre cannot be replayed against the API
// server, and the other way round.
const RunnerAudience = "cadre"

// TokenReviews authenticates in-cluster runners by their projected
// ServiceAccount tokens, asking the API server with a TokenReview through
// c (a controller manager's client, say).
func TokenReviews(c client.Client) runner.Authenticator { return tokenReviews{c: c} }

type tokenReviews struct{ c client.Client }

func (t tokenReviews) Authenticate(ctx context.Context, token string) (runner.Identity, error) {
	review := &authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{RunnerAudience}}}
	if err := t.c.Create(ctx, review); err != nil {
		return runner.Identity{}, fmt.Errorf("token review: %w", err)
	}
	st := review.Status
	if !st.Authenticated || st.User.Username == "" || !slices.Contains(st.Audiences, RunnerAudience) {
		return runner.Identity{}, runner.ErrUnauthenticated
	}
	return runner.Identity{Name: st.User.Username, Expires: expiry(token)}, nil
}

// expiry reads a JWT's exp claim, unverified: the review has vouched for
// the token, and the time only bounds how long its answer is remembered.
func expiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}
