// Package token mints and checks Cadre's bearer tokens: the per-run MCP
// token and a Runner's minted token. A token is
// base64url("<namespace>/<name>") + "." + base64url(32 random bytes): the
// first part locates the object holding the token's SHA-256 without an
// index, and only that hash is ever stored.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

var enc = base64.RawURLEncoding

// Mint returns a fresh token for the object namespace/name, and its hash.
func Mint(namespace, name string) (token, hash string, err error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", "", err
	}
	token = enc.EncodeToString([]byte(namespace+"/"+name)) + "." + enc.EncodeToString(secret)
	return token, Hash(token), nil
}

// Hash is a token's SHA-256, hex.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Locate reads the object a token names. It checks the shape only: whether
// the token is valid is for Matches to say.
func Locate(token string) (namespace, name string, ok bool) {
	where, _, ok := strings.Cut(token, ".")
	if !ok {
		return "", "", false
	}
	raw, err := enc.DecodeString(where)
	if err != nil {
		return "", "", false
	}
	namespace, name, ok = strings.Cut(string(raw), "/")
	if !ok || len(validation.IsDNS1123Label(namespace)) > 0 || len(validation.IsDNS1123Subdomain(name)) > 0 {
		return "", "", false
	}
	return namespace, name, true
}

// Matches reports, in constant time, whether token has the stored hash. An
// empty hash matches nothing.
func Matches(token, hash string) bool {
	return hash != "" && subtle.ConstantTimeCompare([]byte(hash), []byte(Hash(token))) == 1
}
