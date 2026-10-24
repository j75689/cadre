package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// sealer encrypts and authenticates what cadre-server keeps in cookies,
// with a key every replica shares.
type sealer struct{ aead cipher.AEAD }

// ParseSessionKey reads a session key: 32 bytes, raw or hex-encoded (64
// characters, surrounding white space ignored).
func ParseSessionKey(data []byte) ([]byte, error) {
	if s := strings.TrimSpace(string(data)); len(s) == 64 {
		if key, err := hex.DecodeString(s); err == nil {
			return key, nil
		}
	}
	if len(data) == 32 {
		return data, nil
	}
	return nil, errors.New("a session key is 32 bytes, raw or as 64 hex characters")
}

func newSealer(key []byte) (sealer, error) {
	if len(key) != 32 {
		return sealer{}, errors.New("the session key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return sealer{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return sealer{}, err
	}
	return sealer{aead: aead}, nil
}

// seal encodes v as JSON, encrypts it under purpose (so a value sealed for
// one cookie is refused as another) and returns it base64url-encoded.
func (s sealer) seal(purpose string, v any) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := s.aead.Seal(nonce, nonce, plain, []byte(purpose))
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// open reverses seal.
func (s sealer) open(purpose, sealed string, v any) error {
	data, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil || len(data) < s.aead.NonceSize() {
		return errors.New("malformed")
	}
	nonce, box := data[:s.aead.NonceSize()], data[s.aead.NonceSize():]
	plain, err := s.aead.Open(nil, nonce, box, []byte(purpose))
	if err != nil {
		return errors.New("not sealed with this key")
	}
	if err := json.Unmarshal(plain, v); err != nil {
		return fmt.Errorf("sealed value: %w", err)
	}
	return nil
}
