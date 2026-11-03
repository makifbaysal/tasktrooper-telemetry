// Package auth resolves "Authorization: Bearer <api key>" to a project via Postgres api_keys (kind api), or the
// internal service credential (TT_INTERNAL_TOKEN) to the project named in x-trooper-project.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
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

// Internal service credential (contracts §2): the api calls /v1/symbolicate for dashboard users (cookie sessions,
// no api key) with `x-trooper-internal: <TT_INTERNAL_TOKEN>` + `x-trooper-project: <id>`.
const (
	HeaderInternal = "X-Trooper-Internal"
	HeaderProject  = "X-Trooper-Project"
)

// Internal checks the internal token. A nil *Internal (TT_INTERNAL_TOKEN unset) rejects every request.
type Internal struct{ tokenHash [32]byte }

// NewInternal returns nil when token is empty (feature off).
func NewInternal(token string) *Internal {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	return &Internal{tokenHash: sha256.Sum256([]byte(token))}
}

// Project validates token (constant time over sha256 digests, so length does not leak) and parses project.
func (i *Internal) Project(token, project string) (int64, error) {
	if i == nil || token == "" || len(token) > 512 {
		return 0, ErrUnauthorized
	}
	h := sha256.Sum256([]byte(strings.TrimSpace(token)))
	if subtle.ConstantTimeCompare(h[:], i.tokenHash[:]) != 1 {
		return 0, ErrUnauthorized
	}
	pid, err := strconv.ParseInt(strings.TrimSpace(project), 10, 64)
	if err != nil || pid <= 0 || pid > int64(^uint32(0)) {
		return 0, ErrUnauthorized
	}
	return pid, nil
}
