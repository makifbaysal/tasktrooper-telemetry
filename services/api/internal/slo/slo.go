// Package slo has the KPI (SLO) arithmetic: good ratio, burn rate, error budget, status (see .ai/alerting.md § KPI'lar).
package slo

import "math"

// Counts is good/total events over a window.
type Counts struct{ Good, Total float64 }

// Ratio is good/total; 1 when there were no events (no evidence of badness).
func (c Counts) Ratio() float64 {
	if c.Total <= 0 {
		return 1
	}
	return math.Max(0, math.Min(1, c.Good/c.Total))
}

// BurnRate = (1 − good_ratio) / (1 − objective).
func BurnRate(c Counts, objective float64) float64 {
	if objective >= 1 {
		return 0
	}
	return (1 - c.Ratio()) / (1 - objective)
}

// BudgetRemaining is the share of the window's error budget left (0..1).
func BudgetRemaining(window Counts, objective float64) float64 {
	return math.Max(0, math.Min(1, 1-BurnRate(window, objective)))
}

// Status: critical when the budget is gone or it burns ≥ 6× over the last hour (fast burn),
// warning when it burns faster than allowed over 1h (≥ 2×) or 6h (≥ 1×).
func Status(budget, burn1h, burn6h float64) string {
	switch {
	case budget <= 0 || burn1h >= 6:
		return "critical"
	case burn1h >= 2 || burn6h >= 1:
		return "warning"
	}
	return "ok"
}

// RuleState evaluates a multi-window burn-rate rule: firing when both windows burn above threshold,
// pending when only the long one does.
func RuleState(long, short, threshold float64) string {
	switch {
	case long > threshold && short > threshold:
		return "firing"
	case long > threshold:
		return "pending"
	}
	return "ok"
}
