// Package rules holds the pure parts of alert evaluation, shared in shape with apps/web/src/lib/lagAlerts.ts
// (ruleSeries / evaluate / describeRule), so the editor preview and the server agree.
package rules

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

// LagMetrics lists the QueueRule metrics.
var LagMetrics = []string{"lag", "time_behind", "partition_lag", "growing", "not_drained", "rebalances", "oldest_age"}

// ValidLagMetric reports whether m is a QueueRule metric.
func ValidLagMetric(m string) bool {
	for _, x := range LagMetrics {
		if x == m {
			return true
		}
	}
	return false
}

// LagInputs are the per-bucket broker values a queue rule is computed from.
type LagInputs struct {
	Lag          []float64 // total lag of the group (on the rule's topic, or all topics)
	ProduceRate  []float64 // msgs/s written to those topics
	MaxPartition []float64 // biggest single-partition lag
	Rebalances   []float64 // rebalances per hour as reported
	// TimeBehind, when set, is lag ÷ produce rate computed per minute at the source (more exact than per bucket).
	TimeBehind []float64
	// OldestAge is the age of the oldest waiting message in seconds (SQS / Pub/Sub / RabbitMQ; broker_lag_1m.oldest_age_s).
	OldestAge []float64
}

// RuleSeries computes the value the rule compares, per bucket. stepMin is the bucket size in minutes.
func RuleSeries(metric string, in LagInputs, stepMin float64) []float64 {
	n := len(in.Lag)
	out := make([]float64, n)
	switch metric {
	case "lag":
		copy(out, in.Lag)
	case "time_behind":
		if in.TimeBehind != nil {
			copy(out, in.TimeBehind)
			break
		}
		for i := range out {
			if i < len(in.ProduceRate) && in.ProduceRate[i] > 0 {
				out[i] = in.Lag[i] / in.ProduceRate[i]
			} else if in.Lag[i] > 0 {
				// nothing produced but messages waiting: as far behind as the bucket itself
				out[i] = stepMin * 60
			}
		}
	case "partition_lag":
		copy(out, in.MaxPartition)
	case "growing":
		// trend of the smoothed lag (msgs/min), so single noisy points don't count as growth
		const w, k = 5, 4
		smooth := make([]float64, n)
		for i := range in.Lag {
			lo := max(0, i-w+1)
			s := 0.0
			for _, v := range in.Lag[lo : i+1] {
				s += v
			}
			smooth[i] = s / float64(i+1-lo)
		}
		for i := range out {
			if i >= k+w {
				out[i] = (smooth[i] - smooth[i-k]) / (k * stepMin)
			}
		}
	case "not_drained":
		// hours since lag was last below 5% of its peak
		peak := 0.0
		for _, v := range in.Lag {
			peak = math.Max(peak, v)
		}
		if peak == 0 {
			peak = 1
		}
		since := 0.0
		for i, v := range in.Lag {
			if v < peak*0.05 {
				since = 0
			} else {
				since += stepMin / 60
			}
			out[i] = since
		}
	case "rebalances":
		copy(out, in.Rebalances)
	case "oldest_age":
		copy(out, in.OldestAge)
	}
	return out
}

// Threshold is the line the rule compares against; `growing` fires above 0 msgs/min.
func Threshold(r model.QueueRule) float64 {
	if r.Metric == "growing" {
		return 0
	}
	return r.Threshold
}

// Evaluation is the result of replaying a rule over a series.
type Evaluation struct {
	Th        float64
	Episodes  int
	FiringNow bool
	Now       float64
}

// Evaluate counts breach episodes: values above th for at least forMin (one episode per run).
func Evaluate(data []float64, th, forMin, stepMin float64) Evaluation {
	return evaluate(data, forMin, stepMin, th, func(v float64) bool { return v > th })
}

// EvaluateOp is Evaluate with an explicit comparison operator (> < >= <=).
func EvaluateOp(data []float64, op string, th, forMin, stepMin float64) Evaluation {
	cmp := func(v float64) bool { return v > th }
	switch op {
	case "<":
		cmp = func(v float64) bool { return v < th }
	case ">=":
		cmp = func(v float64) bool { return v >= th }
	case "<=":
		cmp = func(v float64) bool { return v <= th }
	}
	return evaluate(data, forMin, stepMin, th, cmp)
}

func evaluate(data []float64, forMin, stepMin, th float64, breach func(float64) bool) Evaluation {
	need := 1
	if stepMin > 0 {
		need = max(1, int(math.Ceil(forMin/stepMin)))
	}
	e := Evaluation{Th: th}
	run := 0
	for i, v := range data {
		if breach(v) {
			run++
		} else {
			run = 0
		}
		if run == need {
			e.Episodes++
		}
		if i == len(data)-1 {
			e.FiringNow = run >= need
			e.Now = v
		}
	}
	return e
}

// Compact formats like the UI's compact(): 18400 → 18.4K.
func Compact(v float64) string {
	a := math.Abs(v)
	switch {
	case a >= 1e9:
		return trim(v/1e9) + "B"
	case a >= 1e6:
		return trim(v/1e6) + "M"
	case a >= 1e3:
		return trim(v/1e3) + "K"
	}
	return trim(v)
}

func trim(v float64) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}

// DescribeQueueRule mirrors describeRule in lagAlerts.ts.
func DescribeQueueRule(r model.QueueRule) string {
	scope := ""
	if r.Topic != "" {
		scope = " on " + r.Topic
	}
	switch r.Metric {
	case "growing":
		return fmt.Sprintf("lag growing for %sm%s", trim(r.ForMin), scope)
	case "not_drained":
		return fmt.Sprintf("lag not drained in %sh%s", trim(r.Threshold), scope)
	}
	v := trim(r.Threshold)
	what := map[string]string{"lag": "lag", "partition_lag": "max partition lag", "time_behind": "time behind", "rebalances": "rebalances",
		"oldest_age": "oldest message age"}[r.Metric]
	switch r.Metric {
	case "lag", "partition_lag":
		v = Compact(r.Threshold)
	case "time_behind", "oldest_age":
		v += "s"
	}
	suffix := ""
	if r.Metric == "rebalances" {
		suffix = "/h"
	}
	dur := ""
	if r.ForMin > 0 {
		dur = fmt.Sprintf(" for %sm", trim(r.ForMin))
	}
	return fmt.Sprintf("%s > %s%s%s%s", what, v, suffix, dur, scope)
}

// DescribeCondition renders a metric monitor condition: "error rate > 2% for 5m".
func DescribeCondition(kind string, metric string, c model.MonitorCondition) string {
	op := c.Op
	if op == "" {
		op = ">"
	}
	val := trim(c.Threshold)
	what := metric
	if kind == "llm" {
		what, val = describeLLM(metric, c)
		s := fmt.Sprintf("%s %s %s", what, op, val)
		if c.ForMin > 0 {
			s += fmt.Sprintf(" for %sm", trim(c.ForMin))
		}
		return s
	}
	switch {
	case kind == "latency" || metric == "p95" || metric == "p99" || metric == "p50":
		if metric == "" || kind == "latency" && metric == "" {
			metric = "p95"
		}
		what, val = metric, trim(c.Threshold)+" ms"
	case metric == "error_rate":
		what, val = "error rate", trim(c.Threshold*100)+"%"
	case metric == "throughput" || metric == "rpm":
		what, val = "throughput", Compact(c.Threshold)+"/min"
	case what == "":
		what = strings.ReplaceAll(kind, "_", " ")
	}
	s := fmt.Sprintf("%s %s %s", what, op, val)
	if c.ForMin > 0 {
		s += fmt.Sprintf(" for %sm", trim(c.ForMin))
	}
	return s
}

// describeLLM names an llm monitor metric and its threshold: "LLM cost > $20/h", "LLM tokens in 1h > 5M".
func describeLLM(metric string, c model.MonitorCondition) (what, val string) {
	win := "5m"
	switch w := c.Window.(type) {
	case string:
		if w != "" {
			win = w
		}
	case float64:
		if w > 0 {
			win = trim(w) + "m"
		}
	}
	money := func(v float64) string {
		if v == math.Trunc(v) {
			return "$" + strconv.FormatFloat(v, 'f', 0, 64)
		}
		return "$" + strconv.FormatFloat(v, 'f', 2, 64)
	}
	switch metric {
	case "cost_usd":
		return "LLM cost in " + win, money(c.Threshold)
	case "cost_usd_per_hour":
		return "LLM cost", money(c.Threshold) + "/h"
	case "tokens", "input_tokens", "output_tokens", "calls", "errors":
		return "LLM " + strings.ReplaceAll(metric, "_", " ") + " in " + win, Compact(c.Threshold)
	case "error_rate":
		return "LLM error rate", trim(c.Threshold*100) + "%"
	case "p95_ms":
		return "LLM p95", trim(c.Threshold) + " ms"
	case "ttft_p95_ms":
		return "LLM time to first token p95", trim(c.Threshold) + " ms"
	}
	return "LLM " + metric, trim(c.Threshold)
}
