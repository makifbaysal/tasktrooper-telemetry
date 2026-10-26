package eval

import (
	"fmt"
	"math"
	"strings"
)

// GridLevels are the quantile levels read from a t-digest to approximate its CDF
// (apdex, latency SLIs). A t-digest state merges at any levels, not just the ones in the column type.
var GridLevels = func() []float64 {
	var l []float64
	for i := 1; i <= 99; i++ {
		l = append(l, float64(i)/100)
	}
	return append(l, 0.995, 0.999)
}()

// GridSQL is GridLevels as a SQL parameter list.
var GridSQL = levelsSQL(GridLevels)

func levelsSQL(ls []float64) string {
	parts := make([]string, len(ls))
	for i, l := range ls {
		parts[i] = fmt.Sprintf("%g", l)
	}
	return strings.Join(parts, ", ")
}

// CDF estimates P(X ≤ x) from quantile values at levels (both ascending) by linear interpolation;
// below the first quantile it interpolates from (0, 0), above the last it returns 1.
func CDF(levels, values []float64, x float64) float64 {
	n := min(len(levels), len(values))
	if n == 0 || math.IsNaN(values[0]) {
		return math.NaN()
	}
	if x < values[0] {
		if values[0] <= 0 {
			return 0
		}
		return levels[0] * math.Max(0, x) / values[0]
	}
	if x >= values[n-1] {
		return 1
	}
	// last index with values[i] ≤ x
	i := 0
	for i+1 < n && values[i+1] <= x {
		i++
	}
	lo, hi := values[i], values[i+1]
	if hi <= lo {
		return levels[i+1]
	}
	return levels[i] + (levels[i+1]-levels[i])*(x-lo)/(hi-lo)
}

// Apdex = (satisfied + tolerating/2) / total with satisfied ≤ T, tolerating ≤ 4T; errors count as frustrated
// (assumed spread evenly over the latency distribution).
func Apdex(levels, values []float64, tMs, errorRatio float64) float64 {
	sat := CDF(levels, values, tMs)
	tol := CDF(levels, values, 4*tMs) - sat
	if math.IsNaN(sat) {
		return math.NaN()
	}
	return (1 - errorRatio) * (sat + tol/2)
}

// ParsePercentile turns "p95", "95", "0.95", "p99.9" into a level; def when empty/invalid.
func ParsePercentile(s string, def float64) float64 {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "p")
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil || f <= 0 {
		return def
	}
	if f >= 1 {
		f /= 100
	}
	if f >= 1 {
		return def
	}
	return f
}
