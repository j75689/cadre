package runner

import (
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/httpjson"
)

// Identity is who a runner authenticated as, and until when that holds.
type Identity struct {
	Name    string    // system:serviceaccount:<namespace>:<name>, say
	Expires time.Time // zero if the credential does not say
}

// An Authenticator turns a runner's bearer token into its identity. It
// fails with ErrUnauthenticated when the token is not valid; any other
// error means it could not tell, and the request may be retried.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (Identity, error)
}

// ErrUnauthenticated means a runner's credential is missing or not valid.
var ErrUnauthenticated = errors.New("unauthenticated")

// Option configures the protocol server.
type Option func(*Server)

// WithAuth makes every request authenticate with auth. builtin is the
// identity of Cadre's own runner: only it may claim builtin agents' tasks.
func WithAuth(auth Authenticator, builtin string) Option {
	return func(s *Server) { s.auth, s.builtin = auth, builtin }
}

type identityKey struct{}

// identityOf is the authenticated identity of a request; empty when the
// server does not authenticate runners.
func identityOf(r *http.Request) string {
	id, _ := r.Context().Value(identityKey{}).(string)
	return id
}

// authenticated wraps h: the request must carry a valid bearer token,
// whose identity goes into its context. Missing or invalid credentials get
// one uniform 401; an authenticator that cannot tell gets 503.
func (s *Server) authenticated(h http.HandlerFunc) http.HandlerFunc {
	if s.auth == nil {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			httpjson.WriteError(w, http.StatusUnauthorized, CodeUnauthenticated, msgUnauthenticated)
			return
		}
		id, err := s.auth.Authenticate(r.Context(), token)
		switch {
		case errors.Is(err, ErrUnauthenticated):
			httpjson.WriteError(w, http.StatusUnauthorized, CodeUnauthenticated, msgUnauthenticated)
			return
		case err != nil:
			s.log.Warn("authenticate a runner", "err", err)
			httpjson.WriteError(w, http.StatusServiceUnavailable, CodeUnavailable, "temporarily unavailable; retry")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id.Name)))
	}
}

const msgUnauthenticated = "a valid runner credential is required"

// claimLabels are the labels a claim is matched with. The cadre.dev/ prefix
// is reserved: on an authenticating server only the builtin identity may
// assert it.
func (s *Server) claimLabels(r *http.Request, labels map[string]string) map[string]string {
	if s.auth == nil || identityOf(r) == s.builtin {
		return labels
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if !strings.HasPrefix(k, v1alpha1.ReservedLabelPrefix) {
			out[k] = v
		}
	}
	return out
}

// Cache bounds.
const (
	cacheTTL     = time.Minute
	cacheEntries = 1024
)

// Cached remembers next's positive answers for a minute, or until the
// credential expires if sooner, by the token's SHA-256, for at most 1024
// tokens. Failures are never remembered.
func Cached(next Authenticator) Authenticator {
	return &cached{next: next, entries: map[[32]byte]*list.Element{}, order: list.New(), now: time.Now}
}

type cached struct {
	next    Authenticator
	mu      sync.Mutex
	entries map[[32]byte]*list.Element
	order   *list.List // most recently used first
	now     func() time.Time
}

type entry struct {
	key   [32]byte
	id    Identity
	until time.Time
}

func (c *cached) Authenticate(ctx context.Context, token string) (Identity, error) {
	key := sha256.Sum256([]byte(token))
	now := c.now()
	c.mu.Lock()
	if el, ok := c.entries[key]; ok {
		e := el.Value.(*entry)
		if now.Before(e.until) {
			c.order.MoveToFront(el)
			c.mu.Unlock()
			return e.id, nil
		}
		c.order.Remove(el)
		delete(c.entries, key)
	}
	c.mu.Unlock()

	id, err := c.next.Authenticate(ctx, token)
	if err != nil {
		return Identity{}, err
	}
	until := now.Add(cacheTTL)
	if !id.Expires.IsZero() && id.Expires.Before(until) {
		until = id.Expires
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.order.Remove(el)
	}
	c.entries[key] = c.order.PushFront(&entry{key: key, id: id, until: until})
	for c.order.Len() > cacheEntries {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*entry).key)
	}
	return id, nil
}
