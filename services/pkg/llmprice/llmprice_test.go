package llmprice

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }

func TestLookup(t *testing.T) {
	tb := New([]Price{
		{Provider: "openai", Model: "gpt-4o", InputPerMTok: 1, OutputPerMTok: 2},
		{Provider: "acme-gw", Model: "house-model", InputPerMTok: 7, OutputPerMTok: 7},
	})
	tests := []struct {
		provider, model string
		wantModel       string
		wantSource      string
		ok              bool
	}{
		{"openai", "gpt-4o", "gpt-4o", "org", true},                                           // org override wins
		{"openai", "gpt-4o-2024-08-06", "gpt-4o", "org", true},                                // prefix on org row
		{"openai", "gpt-4o-mini-2024-07-18", "gpt-4o-mini", "default", true},                  // longest prefix
		{"azure.ai.openai", "gpt-4.1-mini", "gpt-4.1-mini", "default", true},                  // family mapping
		{"openai", "gpt-4-0613", "gpt-4", "default", true},                                    // gpt-4 dated
		{"openai", "gpt-4oz", "", "", false},                                                  // no alnum boundary
		{"anthropic", "claude-sonnet-4-5-20250929", "claude-sonnet-4-5", "default", true},     // dated snapshot
		{"anthropic", "CLAUDE-OPUS-5-5", "claude-opus-5-5", "default", true},                  // case
		{"gcp.vertex_ai", "claude-3-5-sonnet@20240620", "claude-3-5-sonnet", "default", true}, // vertex @ suffix, unknown family falls back
		{"aws.bedrock", "us.anthropic.claude-sonnet-4-20250514-v1:0", "claude-sonnet-4", "default", true},
		{"aws.bedrock", "amazon.nova-pro-v1:0", "nova-pro", "default", true},
		{"gcp.gemini", "models/gemini-2.5-flash", "gemini-2.5-flash", "default", true},
		{"gcp.gemini", "gemini-2.5-flash-lite-preview", "gemini-2.5-flash-lite", "default", true},
		{"acme-gw", "house-model", "house-model", "org", true},
		{"ollama", "llama3", "", "", false},
		{"openai", "", "", "", false},
	}
	for _, tc := range tests {
		p, ok := tb.Lookup(tc.provider, tc.model)
		if ok != tc.ok || p.Model != tc.wantModel || p.Source != tc.wantSource {
			t.Errorf("Lookup(%q, %q) = %q/%q %v, want %q/%q %v", tc.provider, tc.model, p.Model, p.Source, ok, tc.wantModel, tc.wantSource, tc.ok)
		}
	}
}

func TestCost(t *testing.T) {
	p := Price{InputPerMTok: 3, OutputPerMTok: 15, CacheReadPerMTok: f(0.3), CacheWritePerMTok: f(3.75)}
	tests := []struct {
		name string
		p    Price
		u    Usage
		want float64
	}{
		{"plain", p, Usage{Input: 1e6, Output: 1e6}, 18},
		{"cache included in input", p, Usage{Input: 1e6, CacheRead: 5e5, CacheWrite: 1e5}, (4e5*3 + 5e5*0.3 + 1e5*3.75) / 1e6},
		{"uncached clamped at 0", p, Usage{Input: 1e5, CacheRead: 5e5, Output: 10}, (5e5*0.3 + 10*15) / 1e6},
		{"anthropic-shaped sum", p, Usage{Input: 100 + 2000 + 300, CacheRead: 2000, CacheWrite: 300, Output: 50}, (100*3 + 2000*0.3 + 300*3.75 + 50*15) / 1e6},
		{"no cache price → input price", Price{InputPerMTok: 2, OutputPerMTok: 8}, Usage{Input: 1e6, CacheRead: 1e6}, 2},
		{"zero", p, Usage{}, 0},
	}
	for _, tc := range tests {
		if got := tc.p.Cost(tc.u); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: cost %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestListOverridesDefaults(t *testing.T) {
	tb := New([]Price{{Provider: "openai", Model: "GPT-4o", InputPerMTok: 1, OutputPerMTok: 2}})
	n := 0
	for _, p := range tb.List() {
		if p.Provider == "openai" && p.Model == "gpt-4o" {
			n++
			if p.Source != "org" {
				t.Errorf("gpt-4o source %q", p.Source)
			}
		}
	}
	if n != 1 {
		t.Errorf("gpt-4o rows: %d", n)
	}
	if len(tb.List()) != len(Defaults()) || tb.List()[0].Source != "org" {
		t.Errorf("list: %d vs %d", len(tb.List()), len(Defaults()))
	}
	if Updated() == "" {
		t.Error("defaults.json has no updated date")
	}
}

func TestCache(t *testing.T) {
	calls := 0
	fail := false
	c := NewCache(func(_ context.Context, org int64) ([]Price, error) {
		calls++
		if fail {
			return nil, errors.New("pg down")
		}
		return []Price{{Provider: "acme", Model: "m", InputPerMTok: float64(org)}}, nil
	}, time.Minute)
	now := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return now }
	tb, err := c.Table(context.Background(), 7)
	if err != nil || calls != 1 {
		t.Fatalf("first load: %v calls=%d", err, calls)
	}
	if p, ok := tb.Lookup("acme", "m"); !ok || p.InputPerMTok != 7 {
		t.Errorf("org row: %+v %v", p, ok)
	}
	if _, _ = c.Table(context.Background(), 7); calls != 1 {
		t.Errorf("cached: calls=%d", calls)
	}
	now = now.Add(2 * time.Minute)
	fail = true
	tb, err = c.Table(context.Background(), 7)
	if err == nil || calls != 2 {
		t.Errorf("reload error: %v calls=%d", err, calls)
	}
	if _, ok := tb.Lookup("acme", "m"); !ok {
		t.Error("stale table not kept on error")
	}
	tb, err = c.Table(context.Background(), 8)
	if err == nil || tb == nil {
		t.Fatal("unknown org on error: want defaults + error")
	}
	if _, ok := tb.Lookup("anthropic", "claude-sonnet-4-5"); !ok {
		t.Error("defaults missing")
	}
}
