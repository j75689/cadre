package manager

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	v1alpha1 "github.com/j75689/cadre/api/v1alpha1"
	"github.com/j75689/cadre/internal/storage"
)

// A per-run MCP token, sent as "Authorization: Bearer <token>", is
// base64url("<namespace>/<task>") + "." +
// base64url(32 random bytes). The first part locates the task without an
// index; the task's status.run holds the SHA-256 of the whole token, written
// with the claim. The token is bound to that run: when the run ends,
// status.run is cleared and the token stops working.

var enc = base64.RawURLEncoding

// ErrBadToken means an MCP token names no live run: malformed, unknown,
// or its run has ended.
var ErrBadToken = errors.New("token names no active run")

// mintToken returns a fresh token for the task and its hash.
func mintToken(key storage.Key) (token, hash string, err error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", "", err
	}
	token = enc.EncodeToString([]byte(key.Namespace+"/"+key.Name)) + "." + enc.EncodeToString(secret)
	return token, hashToken(token), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// RunByToken resolves an MCP token to its task and run, on a fresh read.
// Every failure, whatever its cause, is ErrBadToken (or a storage error), so
// a caller learns nothing about which part was wrong.
func (m *Manager) RunByToken(ctx context.Context, token string) (*v1alpha1.Task, error) {
	where, _, ok := strings.Cut(token, ".")
	if !ok {
		return nil, ErrBadToken
	}
	raw, err := enc.DecodeString(where)
	if err != nil {
		return nil, ErrBadToken
	}
	ns, name, ok := strings.Cut(string(raw), "/")
	if !ok || len(validation.IsDNS1123Label(ns)) > 0 || len(validation.IsDNS1123Subdomain(name)) > 0 {
		return nil, ErrBadToken
	}
	t, err := m.store.GetTask(ctx, storage.Key{Namespace: ns, Name: name})
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrInvalid) {
		return nil, ErrBadToken
	}
	if err != nil {
		return nil, fmt.Errorf("resolve token: %w", err)
	}
	run := t.Status.Run
	if run == nil || run.MCPTokenHash == "" ||
		subtle.ConstantTimeCompare([]byte(run.MCPTokenHash), []byte(hashToken(token))) != 1 {
		return nil, ErrBadToken
	}
	return t, nil
}
