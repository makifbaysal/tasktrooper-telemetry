package servicemap

import (
	"reflect"
	"testing"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/extract"
)

func TestBest(t *testing.T) {
	known := map[string]bool{"orders-api": true, "payments": true, "shop-web": true, "auth-api": true, "auth-worker": true}
	tests := []struct{ detected, want string }{
		{"orders-api", "orders-api"}, // exact
		{"Orders_API", "orders-api"}, // same normalised name
		{"orders", "orders-api"},     // same stem
		{"payments-service", "payments"},
		{"shop", "shop-web"},
		{"auth", ""},      // auth-api and auth-worker: ambiguous
		{"inventory", ""}, // nothing similar
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.detected, func(t *testing.T) {
			if got := Best(tt.detected, known); got != tt.want {
				t.Fatalf("Best(%q) = %q, want %q", tt.detected, got, tt.want)
			}
		})
	}
	if got := Best("api", map[string]bool{"tt-api": true, "tt-web": true}); got != "tt-api" {
		t.Fatalf("prefixed: got %q", got)
	}
	if got := Best("api", map[string]bool{"orders-api": true, "auth-api": true}); got != "" {
		t.Fatalf("generic name must stay ambiguous: got %q", got)
	}
	if got := Best("orders", nil); got != "" {
		t.Fatalf("no telemetry: got %q", got)
	}
}

func TestLink(t *testing.T) {
	roots := []extract.Root{
		{Path: "services/orders", Detected: "orders", Evidence: extract.EvidenceModule, Kind: "backend", Items: 4},
		{Path: "services/legacy", Detected: "legacy", Evidence: extract.EvidenceModule, Kind: "backend", Items: 2},
		{Path: "apps/web", Detected: "web", Evidence: extract.EvidenceModule, Kind: "web", Items: 3},
		{Path: "tools/seed", Detected: "seed", Evidence: extract.EvidenceModule, Kind: "worker", Items: 1},
	}
	known := map[string]bool{"orders-api": true, "shop-web": true}
	prev := []Mapping{
		{Path: "apps/web", Detected: "web", Service: "shop-web", Status: Confirmed},
		{Path: "tools/seed", Detected: "seed", Status: Ignored},
		{Path: "services/gone", Detected: "gone", Service: "gone-api", Status: Confirmed},
		{Path: "services/legacy", Detected: "legacy", Service: "legacy", Status: Matched},
	}
	tests := []struct {
		name  string
		known map[string]bool
		want  map[string][2]string // path → service, status
	}{
		{"with telemetry", known, map[string][2]string{
			"services/orders": {"orders-api", Matched},
			"services/legacy": {"legacy", Matched}, // quiet service keeps its folder
			"apps/web":        {"shop-web", Confirmed},
			"tools/seed":      {"", Ignored},
			"services/gone":   {"gone-api", Confirmed}, // decision kept without items
		}},
		{"telemetry unreadable", nil, map[string][2]string{
			"services/orders": {"", Unmatched},
			"services/legacy": {"legacy", Matched},
			"apps/web":        {"shop-web", Confirmed},
			"tools/seed":      {"", Ignored},
			"services/gone":   {"gone-api", Confirmed},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := map[string][2]string{}
			for _, m := range Link(roots, prev, tt.known) {
				got[m.Path] = [2]string{m.Service, m.Status}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Link = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOverridesAndChanged(t *testing.T) {
	ms := []Mapping{
		{Path: "services/orders", Detected: "orders", Service: "orders-api", Status: Matched},
		{Path: "services/pay", Detected: "payments", Service: "payments", Status: Matched}, // same name: no override
		{Path: "apps/web", Detected: "web", Service: "shop-web", Status: Confirmed},
		{Path: "tools", Detected: "tools", Status: Ignored},
		{Path: "x", Detected: "x", Status: Unmatched},
	}
	want := []extract.Override{{Path: "services/orders", Name: "orders-api"}, {Path: "apps/web", Name: "shop-web"}}
	if got := Overrides(ms); !reflect.DeepEqual(got, want) {
		t.Fatalf("Overrides = %v, want %v", got, want)
	}
	if got := IgnoredPaths(ms); !reflect.DeepEqual(got, []string{"tools"}) {
		t.Fatalf("IgnoredPaths = %v", got)
	}
	tests := []struct {
		name string
		next []Mapping
		want bool
	}{
		{"same", ms, false},
		{"renamed", []Mapping{{Path: "services/orders", Detected: "orders", Service: "orders-svc", Status: Matched}}, true},
		{"now ignored", []Mapping{{Path: "x", Detected: "x", Status: Ignored}}, true},
		{"unmatched keeps detected name", []Mapping{{Path: "services/pay", Detected: "payments", Status: Unmatched}}, false},
		{"new folder", []Mapping{{Path: "new", Detected: "new", Status: Unmatched}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Changed(ms, tt.next); got != tt.want {
				t.Fatalf("Changed = %v, want %v", got, tt.want)
			}
		})
	}
}
