package manager

import (
	"context"
	"errors"
	"fmt"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/token"
)

// A per-run MCP token, sent as "Authorization: Bearer <token>", is
// base64url("<namespace>/<task>") + "." +
// base64url(32 random bytes). The first part locates the task without an
// index; the task's status.run holds the SHA-256 of the whole token, written
// with the claim. The token is bound to that run: when the run ends,
// status.run is cleared and the token stops working.

// ErrBadToken means an MCP token names no live run: malformed, unknown,
// or its run has ended.
var ErrBadToken = errors.New("token names no active run")

// mintToken returns a fresh token for the task and its hash.
func mintToken(key storage.Key) (tok, hash string, err error) {
	return token.Mint(key.Namespace, key.Name)
}

// RunByToken resolves an MCP token to its task and run, on a fresh read.
// Every failure, whatever its cause, is ErrBadToken (or a storage error), so
// a caller learns nothing about which part was wrong.
func (m *Manager) RunByToken(ctx context.Context, tok string) (*v1alpha1.Task, error) {
	ns, name, ok := token.Locate(tok)
	if !ok {
		return nil, ErrBadToken
	}
	t, err := m.store.GetTask(ctx, storage.Key{Namespace: ns, Name: name})
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrInvalid) {
		return nil, ErrBadToken
	}
	if err != nil {
		return nil, fmt.Errorf("resolve token: %w", err)
	}
	if run := t.Status.Run; run == nil || !token.Matches(tok, run.MCPTokenHash) {
		return nil, ErrBadToken
	}
	return t, nil
}
