package notify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tasktrooper/telemetry/services/pkg/secrets"
)

// SecretResolver replaces "secret:<id>" references in channel config (webhook URLs, routing keys, tokens, header
// values, recipient lists) with their plaintext (services/pkg/secrets, AES-GCM with TT_SECRETS_KEY, AAD = project).
// Resolved values are cached for TTL (60 s) per (project, ref). Plain values and "env:NAME" refs pass through
// (env: is resolved at delivery time).
type SecretResolver struct {
	Store secrets.Store
	Key   []byte
	TTL   time.Duration
	Now   func() time.Time

	mu    sync.Mutex
	cache map[secretKey]cached
}

type secretKey struct {
	project int64
	ref     string
}

type cached struct {
	v   string
	exp time.Time
}

// NewSecretResolver returns a resolver with a 60 s cache.
func NewSecretResolver(st secrets.Store, key []byte) *SecretResolver {
	return &SecretResolver{Store: st, Key: key, TTL: time.Minute, Now: time.Now, cache: map[secretKey]cached{}}
}

// ResolveConfig returns a copy of cfg with every "secret:<id>" string (at any depth) resolved. A missing row or a
// value that does not decrypt is permanent; a database error is retried.
func (r *SecretResolver) ResolveConfig(ctx context.Context, projectID int64, cfg map[string]any) (map[string]any, error) {
	v, err := r.walk(ctx, projectID, cfg)
	if err != nil {
		return nil, err
	}
	return v.(map[string]any), nil
}

func (r *SecretResolver) walk(ctx context.Context, projectID int64, v any) (any, error) {
	switch x := v.(type) {
	case string:
		if !strings.HasPrefix(x, "secret:") {
			return x, nil
		}
		return r.resolve(ctx, projectID, x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			rv, err := r.walk(ctx, projectID, e)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			rv, err := r.walk(ctx, projectID, e)
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	}
	return v, nil
}

func (r *SecretResolver) resolve(ctx context.Context, projectID int64, ref string) (string, error) {
	now := r.Now()
	k := secretKey{projectID, ref}
	r.mu.Lock()
	if c, ok := r.cache[k]; ok && now.Before(c.exp) {
		r.mu.Unlock()
		return c.v, nil
	}
	r.mu.Unlock()
	b, err := secrets.Resolve(ctx, ref, projectID, r.Key, r.Store)
	if err != nil {
		if errors.Is(err, secrets.ErrBadRef) || errors.Is(err, pgx.ErrNoRows) || !isTransient(err) {
			return "", permanent("channel secret %s: %v", ref, err)
		}
		return "", err
	}
	r.mu.Lock()
	r.cache[k] = cached{v: string(b), exp: now.Add(r.TTL)}
	r.mu.Unlock()
	return string(b), nil
}

// isTransient: store errors exposing Transient() bool (store.RetryableError) are retried; everything else
// (bad ref, missing row, decryption failure) is a configuration problem.
func isTransient(err error) bool {
	var t interface{ Transient() bool }
	return errors.As(err, &t) && t.Transient()
}
