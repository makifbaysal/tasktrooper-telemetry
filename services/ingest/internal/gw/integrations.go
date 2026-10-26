package gw

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"

	"github.com/tasktrooper/telemetry/services/ingest/internal/vercel"
	"github.com/tasktrooper/telemetry/services/pkg/secrets"
)

// ErrNoIntegration means the integration id is unknown, disabled or not a Vercel integration.
var ErrNoIntegration = errors.New("integration not found")

// Integrations resolves drain integrations.
type Integrations interface {
	Vercel(ctx context.Context, id int64) (*vercel.Integration, error)
}

// IntegrationLoader loads one integration (uncached).
type IntegrationLoader interface {
	LoadVercel(ctx context.Context, id int64) (*vercel.Integration, error)
}

// IntegrationCache caches resolved integrations (incl. the decrypted drain secret) for 60 s, misses for 10 s,
// and serves a stale entry when Postgres is unreachable.
type IntegrationCache struct {
	src        IntegrationLoader
	ttl, negTT time.Duration
	log        *slog.Logger
	now        func() time.Time

	mu      sync.RWMutex
	entries map[int64]integEntry
	sf      singleflight.Group
}

type integEntry struct {
	in      *vercel.Integration // nil = negative
	expires time.Time
}

// NewIntegrationCache wraps a loader.
func NewIntegrationCache(src IntegrationLoader, log *slog.Logger) *IntegrationCache {
	return &IntegrationCache{src: src, ttl: 60 * time.Second, negTT: 10 * time.Second, log: log, now: time.Now, entries: map[int64]integEntry{}}
}

// Vercel implements Integrations.
func (c *IntegrationCache) Vercel(ctx context.Context, id int64) (*vercel.Integration, error) {
	c.mu.RLock()
	e, ok := c.entries[id]
	c.mu.RUnlock()
	if ok && c.now().Before(e.expires) {
		return hitInteg(e.in)
	}
	v, err, _ := c.sf.Do(strconv.FormatInt(id, 10), func() (any, error) {
		in, err := c.src.LoadVercel(ctx, id)
		switch {
		case errors.Is(err, ErrNoIntegration):
			c.store(id, integEntry{expires: c.now().Add(c.negTT)})
			return (*vercel.Integration)(nil), nil
		case err != nil:
			return nil, err
		}
		c.store(id, integEntry{in: in, expires: c.now().Add(c.ttl)})
		return in, nil
	})
	if err != nil {
		if ok {
			c.log.Warn("integration lookup failed, serving stale entry", "integration", id, "err", err)
			return hitInteg(e.in)
		}
		return nil, err
	}
	return hitInteg(v.(*vercel.Integration))
}

func hitInteg(in *vercel.Integration) (*vercel.Integration, error) {
	if in == nil {
		return nil, ErrNoIntegration
	}
	return in, nil
}

func (c *IntegrationCache) store(id int64, e integEntry) {
	c.mu.Lock()
	if len(c.entries) > 10_000 { // bounded: ids come from the query string
		clear(c.entries)
	}
	c.entries[id] = e
	c.mu.Unlock()
}

// PGIntegrations loads integrations and secrets from Postgres (contracts §10).
type PGIntegrations struct {
	Pool *pgxpool.Pool
	Key  []byte // TT_SECRETS_KEY
	Log  *slog.Logger
}

type vercelConfig struct {
	ProjectMap        map[string]string `json:"projectMap"`
	DrainSecretRef    string            `json:"drainSecretRef"`
	VerifyToken       string            `json:"verifyToken"`
	SpeedInsightsUnit string            `json:"speedInsightsUnit"`
}

// LoadVercel implements IntegrationLoader.
func (p PGIntegrations) LoadVercel(ctx context.Context, id int64) (*vercel.Integration, error) {
	var (
		project int64
		name    string
		raw     []byte
	)
	err := p.Pool.QueryRow(ctx, `SELECT project_id, name, config FROM integrations WHERE id = $1 AND provider = 'vercel' AND status <> 'disabled'`, id).
		Scan(&project, &name, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoIntegration
	}
	if err != nil {
		return nil, err
	}
	var cfg vercelConfig
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
	}
	in := &vercel.Integration{ID: id, ProjectID: project, Name: name, Verify: cfg.VerifyToken, Projects: cfg.ProjectMap,
		VitalsInS: strings.EqualFold(cfg.SpeedInsightsUnit, "s")}
	if cfg.DrainSecretRef != "" {
		sec, err := secrets.Resolve(ctx, cfg.DrainSecretRef, project, p.Key, p)
		if err != nil {
			// The integration exists but its secret can't be resolved: data requests fail (401) until fixed.
			if p.Log != nil {
				p.Log.Warn("vercel drain secret unresolvable", "integration", id, "err", err)
			}
			return in, nil
		}
		in.Secret = sec
	}
	return in, nil
}

// LoadSecret implements secrets.Store.
func (p PGIntegrations) LoadSecret(ctx context.Context, projectID, id int64) (nonce, ciphertext []byte, err error) {
	err = p.Pool.QueryRow(ctx, `SELECT nonce, ciphertext FROM secrets WHERE id = $1 AND project_id = $2`, id, projectID).Scan(&nonce, &ciphertext)
	return nonce, ciphertext, err
}
