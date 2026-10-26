// Package secrets encrypts UI-entered credentials (AES-256-GCM) and resolves credential references
// ("secret:<id>", "env:NAME", "file:/path"). See .ai/contracts.md §10.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// devKey is used when TT_SECRETS_KEY is unset. Never use it outside local development.
var devKey = []byte("tasktrooper-dev-secrets-key-32b!")

// Key returns the 32-byte key from TT_SECRETS_KEY (base64), or the dev key with a warning.
func Key() ([]byte, error) {
	v := os.Getenv("TT_SECRETS_KEY")
	if v == "" {
		slog.Warn("TT_SECRETS_KEY not set; using the development key")
		return devKey, nil
	}
	k, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("secrets: TT_SECRETS_KEY is not base64: %w", err)
	}
	if len(k) != 32 {
		return nil, fmt.Errorf("secrets: TT_SECRETS_KEY must be 32 bytes, got %d", len(k))
	}
	return k, nil
}

// Seal encrypts plaintext for a project. AAD is the project id so a row can't be moved between projects.
func Seal(key []byte, projectID int64, plaintext []byte) (nonce, ciphertext []byte, err error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, gcm.Seal(nil, nonce, plaintext, aad(projectID)), nil
}

// Open decrypts what Seal produced.
func Open(key []byte, projectID int64, nonce, ciphertext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, aad(projectID))
}

func newGCM(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func aad(projectID int64) []byte { return []byte(strconv.FormatInt(projectID, 10)) }

// Store loads an encrypted secret row (nonce, ciphertext) by id, scoped to a project.
type Store interface {
	LoadSecret(ctx context.Context, projectID, id int64) (nonce, ciphertext []byte, err error)
}

// ErrBadRef is returned for unknown reference schemes.
var ErrBadRef = errors.New("secrets: unsupported credentials reference")

// Resolve turns a credentials reference into its plaintext. store may be nil when only env:/file: are used.
func Resolve(ctx context.Context, ref string, projectID int64, key []byte, store Store) ([]byte, error) {
	scheme, rest, ok := strings.Cut(ref, ":")
	if !ok {
		return nil, ErrBadRef
	}
	switch scheme {
	case "env":
		v, ok := os.LookupEnv(rest)
		if !ok {
			return nil, fmt.Errorf("secrets: env %s not set", rest)
		}
		return []byte(v), nil
	case "file":
		b, err := os.ReadFile(rest)
		if err != nil {
			return nil, err
		}
		return []byte(strings.TrimRight(string(b), "\r\n")), nil
	case "secret":
		if store == nil {
			return nil, errors.New("secrets: no store for secret: reference")
		}
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return nil, ErrBadRef
		}
		n, c, err := store.LoadSecret(ctx, projectID, id)
		if err != nil {
			return nil, err
		}
		return Open(key, projectID, n, c)
	}
	return nil, ErrBadRef
}
