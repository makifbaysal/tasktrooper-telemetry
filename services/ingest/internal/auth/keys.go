// Package auth resolves ingest keys to tenants (contracts §2) with an in-memory TTL cache in front of Postgres.
package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"

	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// ErrInvalidKey means the key is unknown, revoked or not an ingest key.
var ErrInvalidKey = errors.New("auth: invalid ingest key")

// Key is a resolved ingest key.
type Key struct {
	ID             int64
	Tenant         tenant.Tenant
	AllowedOrigins []string // empty = any origin
	RateLimitRPS   int      // 0 = default
}

// OriginAllowed reports whether a browser Origin may use this key. Entries may be exact origins
// ("https://shop.example.com"), hosts, or wildcards ("*.example.com", "https://*.example.com").
func (k *Key) OriginAllowed(origin string) bool {
	if len(k.AllowedOrigins) == 0 || origin == "" {
		return true
	}
	host := origin
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	for _, a := range k.AllowedOrigins {
		a = strings.TrimSpace(strings.TrimRight(a, "/"))
		switch {
		case a == "*", strings.EqualFold(a, origin), strings.EqualFold(a, host):
			return true
		case strings.Contains(a, "*."):
			suffix := a[strings.Index(a, "*.")+1:] // ".example.com"
			h := host
			if strings.Contains(a, "://") {
				if !strings.HasPrefix(strings.ToLower(origin), strings.ToLower(a[:strings.Index(a, "://")+3])) {
					continue
				}
			}
			if c := strings.LastIndexByte(h, ':'); c >= 0 && !strings.Contains(suffix, ":") {
				h = h[:c]
			}
			if strings.HasSuffix(strings.ToLower(h), strings.ToLower(suffix)) {
				return true
			}
		}
	}
	return false
}

// Lookup resolves a raw key (by sha256 hash).
type Lookup interface {
	Lookup(ctx context.Context, rawKey string) (*Key, error)
}

// Source loads a key by its sha256 hash; returns ErrInvalidKey when absent.
type Source interface {
	Load(ctx context.Context, hash [32]byte) (*Key, error)
	Touch(ctx context.Context, keyID int64) error
}

type entry struct {
	key     *Key // nil = negative entry
	expires time.Time
}

// Cache is a TTL cache over a Source with negative caching, singleflight loading, stale-on-error
// and rare, asynchronous last_used_at updates.
type Cache struct {
	src        Source
	ttl, negTT time.Duration
	touchEvery time.Duration
	log        *slog.Logger

	mu      sync.RWMutex
	entries map[[32]byte]entry
	sf      singleflight.Group

	touchMu   sync.Mutex
	lastTouch map[int64]time.Time
	touchCh   chan int64

	now func() time.Time
}

// NewCache returns a cache with 60 s TTL, 10 s negative TTL and last_used_at written at most every 5 minutes per key.
func NewCache(src Source, log *slog.Logger) *Cache {
	return &Cache{
		src: src, ttl: 60 * time.Second, negTT: 10 * time.Second, touchEvery: 5 * time.Minute, log: log,
		entries: map[[32]byte]entry{}, lastTouch: map[int64]time.Time{}, touchCh: make(chan int64, 1024),
		now: time.Now,
	}
}

// Run writes last_used_at updates and evicts expired entries until ctx is done.
func (c *Cache) Run(ctx context.Context) {
	sweep := time.NewTicker(time.Minute)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-c.touchCh:
			tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := c.src.Touch(tctx, id); err != nil {
				c.log.Warn("api key touch failed", "key_id", id, "err", err)
			}
			cancel()
		case <-sweep.C:
			now := c.now()
			c.mu.Lock()
			for h, e := range c.entries {
				if now.Sub(e.expires) > 10*time.Minute {
					delete(c.entries, h)
				}
			}
			c.mu.Unlock()
		}
	}
}

// Lookup implements Lookup.
func (c *Cache) Lookup(ctx context.Context, raw string) (*Key, error) {
	if raw == "" || len(raw) > 256 {
		return nil, ErrInvalidKey
	}
	h := sha256.Sum256([]byte(raw))
	now := c.now()
	c.mu.RLock()
	e, ok := c.entries[h]
	c.mu.RUnlock()
	if ok && now.Before(e.expires) {
		return c.hit(e.key)
	}
	v, err, _ := c.sf.Do(string(h[:]), func() (any, error) {
		k, err := c.src.Load(ctx, h)
		switch {
		case errors.Is(err, ErrInvalidKey):
			c.store(h, entry{expires: c.now().Add(c.negTT)})
			return (*Key)(nil), nil
		case err != nil:
			return nil, err
		}
		c.store(h, entry{key: k, expires: c.now().Add(c.ttl)})
		return k, nil
	})
	if err != nil {
		if ok { // Postgres down: serve the stale entry rather than reject traffic
			c.log.Warn("api key lookup failed, serving stale entry", "err", err)
			return c.hit(e.key)
		}
		return nil, err
	}
	return c.hit(v.(*Key))
}

func (c *Cache) store(h [32]byte, e entry) {
	c.mu.Lock()
	c.entries[h] = e
	c.mu.Unlock()
}

func (c *Cache) hit(k *Key) (*Key, error) {
	if k == nil {
		return nil, ErrInvalidKey
	}
	now := c.now()
	c.touchMu.Lock()
	last := c.lastTouch[k.ID]
	due := now.Sub(last) >= c.touchEvery
	if due {
		c.lastTouch[k.ID] = now
	}
	c.touchMu.Unlock()
	if due {
		select {
		case c.touchCh <- k.ID:
		default: // never block the request path
		}
	}
	return k, nil
}

// PG loads keys from Postgres api_keys.
type PG struct{ Pool *pgxpool.Pool }

const loadSQL = `SELECT k.id, p.org_id, k.project_id, k.allowed_origins, COALESCE(k.rate_limit_rps, 0)
FROM api_keys k JOIN projects p ON p.id = k.project_id
WHERE k.hash = $1 AND k.kind = 'ingest' AND k.revoked_at IS NULL`

// Load implements Source.
func (p PG) Load(ctx context.Context, hash [32]byte) (*Key, error) {
	var (
		k            Key
		org, project int64
		rps          int32
	)
	err := p.Pool.QueryRow(ctx, loadSQL, hash[:]).Scan(&k.ID, &org, &project, &k.AllowedOrigins, &rps)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidKey
	}
	if err != nil {
		return nil, err
	}
	k.Tenant = tenant.Tenant{OrgID: uint32(org), ProjectID: uint32(project)}
	k.RateLimitRPS = int(rps)
	return &k, nil
}

// Touch implements Source.
func (p PG) Touch(ctx context.Context, id int64) error {
	_, err := p.Pool.Exec(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, id)
	return err
}

// Static is an in-memory Lookup for tests and benchmarks.
type Static map[string]*Key

// Lookup implements Lookup.
func (s Static) Lookup(_ context.Context, raw string) (*Key, error) {
	if k, ok := s[raw]; ok {
		return k, nil
	}
	return nil, ErrInvalidKey
}
