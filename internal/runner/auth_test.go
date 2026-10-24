package runner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"
)

// counting answers from a map and counts the questions.
type counting struct {
	ids   map[string]Identity
	err   error
	calls int
}

func (c *counting) Authenticate(_ context.Context, token string) (Identity, error) {
	c.calls++
	if c.err != nil {
		return Identity{}, c.err
	}
	id, ok := c.ids[token]
	if !ok {
		return Identity{}, ErrUnauthenticated
	}
	return id, nil
}

func TestCached(t *testing.T) {
	now := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	next := &counting{ids: map[string]Identity{"a": {Name: "sa:a"}, "short": {Name: "sa:short", Expires: now.Add(10 * time.Second)}}}
	c := Cached(next).(*cached)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	for range 3 {
		if id, err := c.Authenticate(ctx, "a"); err != nil || id.Name != "sa:a" {
			t.Fatalf("a: %+v, %v", id, err)
		}
	}
	if next.calls != 1 {
		t.Fatalf("%d reviews for three requests, want 1", next.calls)
	}
	// A refusal is remembered for five seconds; an error that is not a
	// refusal never is.
	for range 2 {
		if _, err := c.Authenticate(ctx, "bad"); !errors.Is(err, ErrUnauthenticated) {
			t.Fatal(err)
		}
	}
	if next.calls != 2 {
		t.Fatalf("%d reviews, want a refusal asked once", next.calls)
	}
	now = now.Add(refusalTTL)
	_, _ = c.Authenticate(ctx, "bad")
	if next.calls != 3 {
		t.Fatalf("%d reviews, want the refusal asked again after five seconds", next.calls)
	}
	next.err = errors.New("the API server is down")
	for range 2 {
		if _, err := c.Authenticate(ctx, "other"); err == nil || errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("an unavailable authenticator: %v", err)
		}
	}
	next.err = nil
	if next.calls != 5 {
		t.Fatalf("%d reviews, want an error never remembered", next.calls)
	}
	next.calls = 3
	// A minute at most, and no longer than the credential holds.
	_, _ = c.Authenticate(ctx, "short")
	now = now.Add(11 * time.Second)
	_, _ = c.Authenticate(ctx, "short")
	if next.calls != 5 {
		t.Fatalf("%d reviews, want the expired credential asked again", next.calls)
	}
	now = now.Add(time.Minute)
	_, _ = c.Authenticate(ctx, "a")
	if next.calls != 6 {
		t.Fatalf("%d reviews, want a minute-old answer asked again", next.calls)
	}

	// At most cacheEntries tokens are kept, the least recently used going.
	for i := range cacheEntries + 10 {
		tok := fmt.Sprintf("t%d", i)
		next.ids[tok] = Identity{Name: tok}
		_, _ = c.Authenticate(ctx, tok)
	}
	if n := c.order.Len(); n != cacheEntries || len(c.entries) != cacheEntries {
		t.Fatalf("%d entries, want %d", n, cacheEntries)
	}
	if _, ok := c.entries[sha("t0")]; ok {
		t.Fatal("the least recently used token was kept")
	}
}

func sha(s string) [32]byte { return sha256.Sum256([]byte(s)) }

func TestBearerToken(t *testing.T) {
	for header, want := range map[string]string{"Bearer abc": "abc", "bearer abc": "abc", "BEARER   abc ": "abc", "Bearer": "", "Basic abc": "", "Bearer a b": "", "": ""} {
		if got, ok := bearerToken(header); got != want || ok != (want != "") {
			t.Errorf("%q: %q, %v; want %q", header, got, ok, want)
		}
	}
}
