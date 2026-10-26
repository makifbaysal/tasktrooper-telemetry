package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

func TestOriginAllowed(t *testing.T) {
	k := &Key{AllowedOrigins: []string{"https://shop.example.com", "app.example.net", "*.example.org", "https://*.secure.io"}}
	tests := []struct {
		origin string
		want   bool
	}{
		{"", true},
		{"https://shop.example.com", true},
		{"http://shop.example.com", false},
		{"https://app.example.net", true},
		{"https://a.example.org", true},
		{"http://b.c.example.org:8080", true},
		{"https://example.org.evil.com", false},
		{"https://x.secure.io", true},
		{"http://x.secure.io", false},
		{"https://evil.com", false},
	}
	for _, tt := range tests {
		if got := k.OriginAllowed(tt.origin); got != tt.want {
			t.Errorf("OriginAllowed(%q)=%v want %v", tt.origin, got, tt.want)
		}
	}
	if !(&Key{}).OriginAllowed("https://anything") {
		t.Error("empty allowlist allows any origin")
	}
}

type fakeSource struct {
	loads, touches atomic.Int32
	fail           atomic.Bool
	keys           map[[32]byte]*Key
}

func (f *fakeSource) Load(_ context.Context, h [32]byte) (*Key, error) {
	f.loads.Add(1)
	if f.fail.Load() {
		return nil, errors.New("pg down")
	}
	if k, ok := f.keys[h]; ok {
		return k, nil
	}
	return nil, ErrInvalidKey
}

func (f *fakeSource) Touch(context.Context, int64) error { f.touches.Add(1); return nil }

func TestCache(t *testing.T) {
	src := &fakeSource{keys: map[[32]byte]*Key{sha256.Sum256([]byte("k1")): {ID: 1, Tenant: tenant.Tenant{OrgID: 1, ProjectID: 1}}}}
	c := NewCache(src, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	for range 3 {
		if k, err := c.Lookup(ctx, "k1"); err != nil || k.ID != 1 {
			t.Fatal(k, err)
		}
	}
	if src.loads.Load() != 1 {
		t.Fatalf("loads=%d want 1 (cached)", src.loads.Load())
	}
	// Negative caching.
	for range 3 {
		if _, err := c.Lookup(ctx, "bad"); !errors.Is(err, ErrInvalidKey) {
			t.Fatal(err)
		}
	}
	if src.loads.Load() != 2 {
		t.Fatalf("loads=%d want 2 (negative cached)", src.loads.Load())
	}
	// Expiry → reload; Postgres down → stale entry served.
	now = now.Add(61 * time.Second)
	src.fail.Store(true)
	if k, err := c.Lookup(ctx, "k1"); err != nil || k.ID != 1 {
		t.Fatalf("stale on error: %v %v", k, err)
	}
	if _, err := c.Lookup(ctx, "never-seen"); err == nil || errors.Is(err, ErrInvalidKey) {
		t.Fatalf("unknown key with backend down must be a backend error, got %v", err)
	}
	// Touch is enqueued once per touchEvery.
	if len(c.touchCh) != 1 {
		t.Fatalf("touch queue=%d want 1", len(c.touchCh))
	}
	if _, err := c.Lookup(ctx, ""); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("empty key must be invalid")
	}
}
