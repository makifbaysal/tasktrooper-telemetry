package burn

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestRate(t *testing.T) {
	tests := []struct {
		name string
		c    Counts
		obj  float64
		want float64
	}{
		{"no traffic", Counts{0, 0}, 0.99, 0},
		{"perfect", Counts{100, 100}, 0.99, 0},
		{"exactly on budget", Counts{99, 100}, 0.99, 1},
		{"6x", Counts{94, 100}, 0.99, 6},
		{"14.4x of 99.9%", Counts{98.56, 100}, 0.999, 14.4},
		{"all bad 95%", Counts{0, 10}, 0.95, 20},
		{"good > total clamps", Counts{120, 100}, 0.9, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Rate(tc.c, tc.obj); !near(got, tc.want) {
				t.Fatalf("Rate = %v want %v", got, tc.want)
			}
		})
	}
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name string
		w    Window
		obj  float64
		want bool
	}{
		{"fast burn both windows", Window{Counts{850, 1000}, Counts{80, 100}, 14.4}, 0.99, true},
		{"long only (burn stopped)", Window{Counts{850, 1000}, Counts{100, 100}, 14.4}, 0.99, false},
		{"short only (blip)", Window{Counts{990, 1000}, Counts{50, 100}, 14.4}, 0.99, false},
		{"slow burn 6x", Window{Counts{930, 1000}, Counts{92, 100}, 6}, 0.99, true},
		{"equal is not above", Window{Counts{94, 100}, Counts{94, 100}, 6}, 0.99, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if r := Check(tc.w, tc.obj); r.Breached != tc.want {
				t.Fatalf("Check = %+v want %v", r, tc.want)
			}
		})
	}
}

func TestBudget(t *testing.T) {
	if got := BudgetRemaining(Counts{995, 1000}, 0.99); !near(got, 0.5) {
		t.Fatalf("budget %v", got)
	}
	if got := BudgetRemaining(Counts{0, 0}, 0.99); got != 1 {
		t.Fatalf("empty budget %v", got)
	}
	// 6× on a 28-day window ≈ 4.7 days (alerting.md).
	if d := TimeToExhaustion(6) * 28; math.Abs(d-4.666) > 0.01 {
		t.Fatalf("exhaustion %v", d)
	}
	if !math.IsNaN(Counts{}.Ratio()) {
		t.Fatal("ratio of empty should be NaN")
	}
}
