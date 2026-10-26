// Package auth resolves `Authorization: Bearer <api key>` to a tenant (api_keys.kind = 'api', sha256 hash).
package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// ErrUnauthorized is returned for unknown, revoked or non-api keys.
var ErrUnauthorized = errors.New("unauthorized")

// Resolver maps an API key to its tenant.
type Resolver interface {
	Resolve(ctx context.Context, key string) (tenant.Tenant, error)
}

type entry struct {
	t   tenant.Tenant
	err error
	exp time.Time
}

// PG resolves keys against Postgres with a small TTL cache (negative results are cached too).
type PG struct {
	Pool *pgxpool.Pool
	TTL  time.Duration

	mu    sync.Mutex
	cache map[[32]byte]entry
}

// NewPG returns a resolver with a 60 s cache.
func NewPG(pool *pgxpool.Pool) *PG {
	return &PG{Pool: pool, TTL: time.Minute, cache: map[[32]byte]entry{}}
}

// Resolve implements Resolver. This is the only api query without a tenant filter: it is how the tenant is found.
func (p *PG) Resolve(ctx context.Context, key string) (tenant.Tenant, error) {
	if key == "" {
		return tenant.Tenant{}, ErrUnauthorized
	}
	h := sha256.Sum256([]byte(key))
	p.mu.Lock()
	if e, ok := p.cache[h]; ok && time.Now().Before(e.exp) {
		p.mu.Unlock()
		return e.t, e.err
	}
	p.mu.Unlock()
	var org, project int64
	err := p.Pool.QueryRow(ctx, `SELECT p.org_id, p.id FROM api_keys k JOIN projects p ON p.id = k.project_id
	  WHERE k.hash = $1 AND k.kind = 'api' AND k.revoked_at IS NULL`, h[:]).Scan(&org, &project)
	e := entry{exp: time.Now().Add(p.TTL)}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		e.err = ErrUnauthorized
	case err != nil:
		return tenant.Tenant{}, err // transient: don't cache
	default:
		e.t = tenant.Tenant{OrgID: uint32(org), ProjectID: uint32(project)}
		go p.touch(h[:])
	}
	p.mu.Lock()
	if len(p.cache) > 10000 {
		p.cache = map[[32]byte]entry{}
	}
	p.cache[h] = e
	p.mu.Unlock()
	return e.t, e.err
}

func (p *PG) touch(hash []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = p.Pool.Exec(ctx, `UPDATE api_keys SET last_used_at = now() WHERE hash = $1`, hash)
}

// Static resolves a fixed key set (tests).
type Static map[string]tenant.Tenant

// Resolve implements Resolver.
func (s Static) Resolve(_ context.Context, key string) (tenant.Tenant, error) {
	if t, ok := s[key]; ok {
		return t, nil
	}
	return tenant.Tenant{}, ErrUnauthorized
}
