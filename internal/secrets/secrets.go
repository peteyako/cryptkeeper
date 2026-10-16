// Package secrets seals exchange API secrets at rest with AES-256-GCM.
//
// Sealed values look like "enc:v1:<base64(nonce||ciphertext)>". The key comes from
// VAULT_SECRET_KEY (base64, 32 bytes) or, if unset, from data/secret.key, which is
// generated on first use with mode 0600. This protects a leaked sources.yaml or backup;
// it does not protect against someone who can also read the key file.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const prefix = "enc:v1:"

// Box seals and opens secrets with one key.
type Box struct{ aead cipher.AEAD }

// NewBox builds a Box from a 32-byte key.
func NewBox(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secret key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// LoadKey returns the key from envKey (base64) if set, else reads or creates keyFile.
func LoadKey(envKey, keyFile string) ([]byte, error) {
	if envKey != "" {
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(envKey))
		if err != nil {
			return nil, fmt.Errorf("VAULT_SECRET_KEY: %w", err)
		}
		return k, nil
	}
	raw, err := os.ReadFile(keyFile)
	if err == nil {
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", keyFile, err)
		}
		return k, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(k)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

// Seal encrypts plain. An empty string stays empty.
func (b *Box) Seal(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := b.aead.Seal(nonce, nonce, []byte(plain), []byte(prefix))
	return prefix + base64.StdEncoding.EncodeToString(ct), nil
}

// Open decrypts a sealed value. Values without the prefix are returned unchanged, so a
// hand-edited sources.yaml with a plaintext secret still works (it is re-sealed on save).
func (b *Box) Open(sealed string) (string, error) {
	rest, ok := strings.CutPrefix(sealed, prefix)
	if !ok {
		return sealed, nil
	}
	raw, err := base64.StdEncoding.DecodeString(rest)
	if err != nil {
		return "", fmt.Errorf("sealed secret: %w", err)
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("sealed secret: too short")
	}
	plain, err := b.aead.Open(nil, raw[:ns], raw[ns:], []byte(prefix))
	if err != nil {
		return "", errors.New("sealed secret: wrong key or corrupted value")
	}
	return string(plain), nil
}

// IsSealed reports whether s is a sealed value.
func IsSealed(s string) bool { return strings.HasPrefix(s, prefix) }

// Mask shows the last 4 characters of a secret, e.g. "••••3f9a".
func Mask(plain string) string {
	if plain == "" {
		return ""
	}
	r := []rune(plain)
	if len(r) <= 4 {
		return "••••"
	}
	return "••••" + string(r[len(r)-4:])
}
