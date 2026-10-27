package llmprice

import (
	"context"
	"sync"
	"time"
)

// Loader reads one org's override rows (Postgres llm_prices). It lives in the caller so this package stays free of
// database drivers.
type Loader func(ctx context.Context, orgID int64) ([]Price, error)

// Cache keeps one Table per org for ttl. A failed load keeps serving the last table (or the defaults) and returns
// the error, so cost estimates never fail a caller.
type Cache struct {
	load Loader
	ttl  time.Duration
	now  func() time.Time

	mu sync.Mutex
	m  map[int64]cached
}

type cached struct {
	t  *Table
	at time.Time
}

// NewCache returns a cache; ttl ≤ 0 means one minute.
func NewCache(load Loader, ttl time.Duration) *Cache {
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &Cache{load: load, ttl: ttl, now: time.Now, m: map[int64]cached{}}
}

// Table returns the org's table (defaults + overrides). It is never nil.
func (c *Cache) Table(ctx context.Context, orgID int64) (*Table, error) {
	now := c.now()
	c.mu.Lock()
	e, ok := c.m[orgID]
	c.mu.Unlock()
	if ok && now.Sub(e.at) < c.ttl {
		return e.t, nil
	}
	if c.load == nil {
		return New(nil), nil
	}
	rows, err := c.load(ctx, orgID)
	if err != nil {
		if ok {
			return e.t, err
		}
		return New(nil), err
	}
	t := New(rows)
	c.mu.Lock()
	c.m[orgID] = cached{t: t, at: now}
	c.mu.Unlock()
	return t, nil
}
