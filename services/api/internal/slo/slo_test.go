package slo

import (
	"math"
	"testing"
)

func TestBurn(t *testing.T) {
	for _, tc := range []struct {
		c         Counts
		objective float64
		burn      float64
		budget    float64
	}{
		{Counts{95, 100}, 0.95, 1, 0},
		{Counts{99, 100}, 0.95, 0.2, 0.8},
		{Counts{0, 0}, 0.99, 0, 1},
		{Counts{68, 100}, 0.95, 6.4, 0},
	} {
		if b := BurnRate(tc.c, tc.objective); math.Abs(b-tc.burn) > 1e-9 {
			t.Errorf("burn %v = %v want %v", tc.c, b, tc.burn)
		}
		if b := BudgetRemaining(tc.c, tc.objective); math.Abs(b-tc.budget) > 1e-9 {
			t.Errorf("budget %v = %v want %v", tc.c, b, tc.budget)
		}
	}
}

func TestStatus(t *testing.T) {
	for _, tc := range []struct {
		budget, b1, b6 float64
		want           string
	}{
		{0.5, 0.5, 0.5, "ok"}, {0.5, 2.5, 0.5, "warning"}, {0.5, 0.5, 1.2, "warning"}, {0.5, 6.4, 1, "critical"}, {0, 0, 0, "critical"},
	} {
		if got := Status(tc.budget, tc.b1, tc.b6); got != tc.want {
			t.Errorf("%+v → %s", tc, got)
		}
	}
	if RuleState(7, 8, 6) != "firing" || RuleState(7, 1, 6) != "pending" || RuleState(1, 9, 6) != "ok" {
		t.Error("rule state")
	}
}
