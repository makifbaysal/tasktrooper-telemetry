// Package auth resolves "Authorization: Bearer <api key>" to a project via Postgres api_keys (kind api).
package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUnauthorized is returned for missing, unknown or revoked keys.
var ErrUnauthorized = errors.New("unauthorized")

// Lookup resolves a sha256(key) to a project id.
type Lookup func(ctx context.Context, hash []byte) (project int64, err error)

// PostgresLookup queries api_keys (deploy/postgres/001_init.sql).
func PostgresLookup(pool *pgxpool.Pool) Lookup {
	return func(ctx context.Context, hash []byte) (int64, error) {
		var project int64
		err := pool.QueryRow(ctx,
			`SELECT project_id FROM api_keys WHERE hash = $1 AND kind = 'api' AND revoked_at IS NULL`, hash).Scan(&project)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrUnauthorized
		}
		return project, err
	}
}

// Authenticator caches lookups (positive and negative) for TTL.
type Authenticator struct {
	lookup Lookup
	ttl    time.Duration
	now    func() time.Time

	mu    sync.Mutex
	cache map[[32]byte]entry
}

type entry struct {
	project int64
	ok      bool
	exp     time.Time
}

// New returns an Authenticator with the given cache TTL (contracts.md §2: 60 s).
func New(lookup Lookup, ttl time.Duration) *Authenticator {
	return &Authenticator{lookup: lookup, ttl: ttl, now: time.Now, cache: map[[32]byte]entry{}}
}

// BearerKey extracts the key from the Authorization header.
func BearerKey(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// Project resolves a key. Infrastructure errors are returned as-is (callers answer 503) and not cached.
func (a *Authenticator) Project(ctx context.Context, key string) (int64, error) {
	if key == "" || len(key) > 512 {
		return 0, ErrUnauthorized
	}
	h := sha256.Sum256([]byte(key))
	now := a.now()
	a.mu.Lock()
	if e, ok := a.cache[h]; ok && now.Before(e.exp) {
		a.mu.Unlock()
		if !e.ok {
			return 0, ErrUnauthorized
		}
		return e.project, nil
	}
	if len(a.cache) > 10000 { // bound memory under key-guessing
		for k, e := range a.cache {
			if !now.Before(e.exp) {
				delete(a.cache, k)
			}
		}
	}
	a.mu.Unlock()
	project, err := a.lookup(ctx, h[:])
	if err != nil && !errors.Is(err, ErrUnauthorized) {
		return 0, err
	}
	a.mu.Lock()
	a.cache[h] = entry{project: project, ok: err == nil, exp: now.Add(a.ttl)}
	a.mu.Unlock()
	return project, err
}
