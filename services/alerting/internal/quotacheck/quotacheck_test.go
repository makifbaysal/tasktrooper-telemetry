package quotacheck

import (
	"fmt"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/aiquota"
)

func TestPlan(t *testing.T) {
	plans, _ := aiquota.ParsePlans("")
	ptr := func(v int64) *int64 { return &v }
	slack := []string{"slack:C0123ABCD#ai"}
	tests := []struct {
		name string
		org  Org
		want string // "threshold mark" or "" (no alert)
	}{
		{"below", Org{Plan: "team", Used: 3999, Targets: slack}, ""},
		{"80 on team", Org{Plan: "team", Used: 4012, Targets: slack}, "80 [80]"},
		{"100 jump", Org{Plan: "free", Used: 120, Targets: slack}, "100 [80 100]"},
		{"override", Org{Plan: "free", Override: ptr(1000), Used: 850, Targets: slack}, "80 [80]"},
		{"unlimited", Org{Plan: "enterprise", Used: 1e9, Targets: slack}, ""},
		{"override unlimited", Org{Plan: "free", Override: ptr(-1), Used: 1e9, Targets: slack}, ""},
		{"no targets", Org{Plan: "free", Used: 500}, ""},
		{"zero limit", Org{Plan: "free", Override: ptr(0), Used: 0.5, Targets: slack}, "100 [80 100]"},
	}
	for _, tt := range tests {
		got := ""
		if a := Plan([]Org{tt.org}, plans); len(a) == 1 {
			got = fmt.Sprintf("%d %v", a[0].Threshold, a[0].Mark)
		}
		if got != tt.want {
			t.Errorf("%s: got %q want %q", tt.name, got, tt.want)
		}
	}
}

func TestBuildPayload(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	a := Alert{Org: Org{ID: 3, ProjectID: 9, Used: 4012.345, Targets: []string{"a", "a", "", "b"}}, Limit: 5000, Threshold: 80}
	p := BuildPayload(a, now, "https://app.example.com")
	if p.OrgID != 3 || p.ProjectID != 9 || p.Period != "2026-10" || p.Threshold != 80 || p.Used != 4012.35 || p.Limit != 5000 ||
		p.ResetsAt != time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).UnixMilli() || p.Kind != "ai_quota" || p.Link != "https://app.example.com/settings/usage" {
		t.Fatalf("%+v", p)
	}
	if got := fmt.Sprint(a.Targets()); got != "[a b]" {
		t.Fatalf("targets %s", got)
	}
	if k := DedupeKey(3, "2026-10", 80, "slack:C1#x"); k != "ai_quota:3:2026-10:80:slack:C1#x" {
		t.Fatal(k)
	}
}
