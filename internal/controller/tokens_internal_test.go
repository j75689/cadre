package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
)

// mintedAt is when the status first had the token's hash: a new hash, one
// written into the Secret by hand say, is a new token, minted then.
func TestSetTokenMintedAt(t *testing.T) {
	rn := &v1alpha1.Runner{ObjectMeta: metav1.ObjectMeta{Name: "laptop"}}
	o := tokenOwner{obj: rn, kind: "Runner", secret: "laptop-token", status: &rn.Status.MintedToken}
	long := metav1.NewTime(time.Now().Add(-time.Hour).UTC().Truncate(time.Second))
	rn.Status.TokenSecret, rn.Status.TokenHash, rn.Status.MintedAt = "laptop-token", "aaaa", &long

	if setToken(o, "aaaa") || !rn.Status.MintedAt.Equal(&long) {
		t.Fatalf("the same hash: mintedAt %v", rn.Status.MintedAt)
	}
	if !setToken(o, "bbbb") || rn.Status.MintedAt == nil || !rn.Status.MintedAt.After(long.Time) || rn.Status.TokenHash != "bbbb" {
		t.Fatalf("a new hash: %+v", rn.Status.MintedToken)
	}
	if !setToken(o, "") || rn.Status.MintedAt != nil || rn.Status.TokenSecret != "" {
		t.Fatalf("no token: %+v", rn.Status.MintedToken)
	}
}
