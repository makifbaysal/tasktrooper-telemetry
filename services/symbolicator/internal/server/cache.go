package server

import (
	"container/list"
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ResultCache caches symbolication results. Implementations must tolerate an unavailable backend.
type ResultCache interface {
	// Gen returns the artifact generation of a scope; uploads bump it so cached results are not stale.
	Gen(ctx context.Context, scope string) string
	Bump(ctx context.Context, scope string)
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration)
}

// RedisCache implements ResultCache on Redis; errors degrade to cache misses.
type RedisCache struct{ C *redis.Client }

func (r RedisCache) Gen(ctx context.Context, scope string) string {
	v, err := r.C.Get(ctx, "ttsym:gen:"+scope).Result()
	if err != nil {
		return "0"
	}
	return v
}

func (r RedisCache) Bump(ctx context.Context, scope string) {
	r.C.Incr(ctx, "ttsym:gen:"+scope)
}

func (r RedisCache) Get(ctx context.Context, key string) ([]byte, bool) {
	v, err := r.C.Get(ctx, key).Bytes()
	return v, err == nil
}

func (r RedisCache) Set(ctx context.Context, key string, val []byte, ttl time.Duration) {
	r.C.Set(ctx, key, val, ttl)
}

// MemCache is a process-local ResultCache (tests, or when Redis is not configured).
type MemCache struct {
	mu   sync.Mutex
	gens map[string]int
	vals map[string][]byte
}

func NewMemCache() *MemCache { return &MemCache{gens: map[string]int{}, vals: map[string][]byte{}} }

func (m *MemCache) Gen(_ context.Context, scope string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strconv.Itoa(m.gens[scope])
}

func (m *MemCache) Bump(_ context.Context, scope string) {
	m.mu.Lock()
	m.gens[scope]++
	m.mu.Unlock()
}

func (m *MemCache) Get(_ context.Context, key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vals[key]
	return v, ok
}

func (m *MemCache) Set(_ context.Context, key string, val []byte, _ time.Duration) {
	m.mu.Lock()
	if len(m.vals) > 10000 {
		m.vals = map[string][]byte{}
	}
	m.vals[key] = val
	m.mu.Unlock()
}

// lru is a small generic LRU with an eviction hook (closes native symbolizers, removes temp files).
type lru[V any] struct {
	mu      sync.Mutex
	cap     int
	ll      *list.List
	items   map[string]*list.Element
	onEvict func(key string, v V)
}

type lruItem[V any] struct {
	key string
	v   V
}

func newLRU[V any](capacity int, onEvict func(string, V)) *lru[V] {
	return &lru[V]{cap: capacity, ll: list.New(), items: map[string]*list.Element{}, onEvict: onEvict}
}

func (l *lru[V]) get(key string) (V, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.items[key]; ok {
		l.ll.MoveToFront(e)
		return e.Value.(*lruItem[V]).v, true
	}
	var zero V
	return zero, false
}

// add stores v; when key is already present the existing value wins and v is evicted.
func (l *lru[V]) add(key string, v V) V {
	l.mu.Lock()
	if e, ok := l.items[key]; ok {
		l.ll.MoveToFront(e)
		cur := e.Value.(*lruItem[V]).v
		l.mu.Unlock()
		if l.onEvict != nil {
			l.onEvict(key, v)
		}
		return cur
	}
	l.items[key] = l.ll.PushFront(&lruItem[V]{key: key, v: v})
	var evicted []*lruItem[V]
	for l.ll.Len() > l.cap {
		back := l.ll.Back()
		l.ll.Remove(back)
		it := back.Value.(*lruItem[V])
		delete(l.items, it.key)
		evicted = append(evicted, it)
	}
	l.mu.Unlock()
	if l.onEvict != nil {
		for _, it := range evicted {
			l.onEvict(it.key, it.v)
		}
	}
	return v
}
