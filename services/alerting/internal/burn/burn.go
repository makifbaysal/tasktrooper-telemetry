// Package burn implements SLO error-budget burn-rate math (multi-window, multi-burn-rate; ADR-008).
package burn

import "math"

// Counts are good/total events over a window.
type Counts struct{ Good, Total float64 }

// Ratio is the good-event ratio; NaN without traffic.
func (c Counts) Ratio() float64 {
	if c.Total <= 0 {
		return math.NaN()
	}
	return math.Min(1, math.Max(0, c.Good/c.Total))
}

// Rate is (1 − good/total) / (1 − objective). No traffic burns nothing.
func Rate(c Counts, objective float64) float64 {
	if c.Total <= 0 || objective >= 1 {
		return 0
	}
	return (1 - c.Ratio()) / (1 - objective)
}

// Window is one burn-rate alert pair: both the long and the short window must burn faster than Factor.
// The short window makes the alert reset quickly once the burn stops.
type Window struct {
	Long, Short Counts
	Factor      float64
}

// Result of a multi-window check.
type Result struct {
	LongRate, ShortRate float64
	Breached            bool
}

// Check evaluates w against objective.
func Check(w Window, objective float64) Result {
	l, s := Rate(w.Long, objective), Rate(w.Short, objective)
	return Result{LongRate: l, ShortRate: s, Breached: l > w.Factor && s > w.Factor}
}

// BudgetRemaining is the share of the window's error budget left (can go negative when overspent).
func BudgetRemaining(window Counts, objective float64) float64 {
	if window.Total <= 0 {
		return 1
	}
	allowed := (1 - objective) * window.Total
	if allowed <= 0 {
		return 0
	}
	bad := window.Total - window.Good
	return 1 - bad/allowed
}

// TimeToExhaustion is how long (in units of the SLO window) a constant burn rate takes to spend a full budget.
func TimeToExhaustion(rate float64) float64 {
	if rate <= 0 {
		return math.Inf(1)
	}
	return 1 / rate
}
