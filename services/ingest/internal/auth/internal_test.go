package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

type countingProjects struct {
	m     StaticProjects
	calls atomic.Int32
	fail  atomic.Bool
}

func (c *countingProjects) ProjectOrg(ctx context.Context, id int64) (int64, error) {
	c.calls.Add(1)
	if c.fail.Load() {
		return 0, errors.New("pg down")
	}
	return c.m.ProjectOrg(ctx, id)
}

func TestInternalLookup(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if NewInternal("  ", nil, log) != nil {
		t.Fatal("empty token must disable the feature")
	}
	var off *Internal
	if _, err := off.Lookup(context.Background(), "x", "1"); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("nil Internal rejects")
	}
	src := &countingProjects{m: StaticProjects{9601: 5}}
	in := NewInternal("tok", src, log)
	now := time.Unix(1000, 0)
	in.now = func() time.Time { return now }

	tests := []struct {
		token, project string
		wantOrg        uint32
		wantErr        bool
	}{
		{"tok", "9601", 5, false},
		{"tok", " 9601 ", 5, false},
		{"tok2", "9601", 0, true},
		{"to", "9601", 0, true},
		{"", "9601", 0, true},
		{"tok", "", 0, true},
		{"tok", "-1", 0, true},
		{"tok", "99999999999", 0, true},
		{"tok", "9602", 0, true},
	}
	for _, tt := range tests {
		k, err := in.Lookup(context.Background(), tt.token, tt.project)
		if tt.wantErr {
			if !errors.Is(err, ErrInvalidKey) {
				t.Errorf("%q/%q: want ErrInvalidKey, got %v", tt.token, tt.project, err)
			}
			continue
		}
		if err != nil || k.Tenant.OrgID != tt.wantOrg || k.Tenant.ProjectID != 9601 || !k.Internal {
			t.Errorf("%q/%q: %+v %v", tt.token, tt.project, k, err)
		}
	}
	if c := src.calls.Load(); c != 2 { // 9601 once (cached), 9602 once (negative)
		t.Fatalf("source calls = %d, want 2", c)
	}
	// Stale on error after TTL.
	now = now.Add(2 * time.Minute)
	src.fail.Store(true)
	if k, err := in.Lookup(context.Background(), "tok", "9601"); err != nil || k.Tenant.OrgID != 5 {
		t.Fatalf("stale: %+v %v", k, err)
	}
	// Backend error without cached entry surfaces as a non-auth error.
	if _, err := in.Lookup(context.Background(), "tok", "9700"); err == nil || errors.Is(err, ErrInvalidKey) {
		t.Fatalf("backend error: %v", err)
	}
}
