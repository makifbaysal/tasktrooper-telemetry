package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/pkg/llmprice"
)

// Monitor kind llm (ADR-023, contracts §25): validation and the editor preview. The alert engine evaluates it
// (services/alerting/internal/eval/llm.go); the preview mirrors its formulas per bucket.

// LLMMetrics are the llm monitor metrics.
var LLMMetrics = []string{"cost_usd", "cost_usd_per_hour", "tokens", "input_tokens", "output_tokens", "calls", "errors", "error_rate", "p95_ms", "ttft_p95_ms"}

var llmFilterKeys = map[string]bool{"service": true, "provider": true, "model": true, "route": true, "method": true, "operation": true}

// validateLLMQuery checks {filter: {service?, provider?, model?, route?, method?, operation?}, metric}.
func validateLLMQuery(raw json.RawMessage) error {
	var q struct {
		Filter json.RawMessage `json:"filter"`
		Metric string          `json:"metric"`
	}
	if err := strictDecode(raw, &q); err != nil {
		return Invalid("llm query must be {filter?, metric}: " + err.Error())
	}
	valid := false
	for _, m := range LLMMetrics {
		valid = valid || m == q.Metric
	}
	if !valid {
		return Invalid("query.metric must be " + strings.Join(LLMMetrics, "|"))
	}
	_, err := stringFilter(q.Filter, llmFilterKeys)
	return err
}

type llmPreviewQuery struct {
	Metric string `json:"metric"`
	Filter struct {
		Service, Provider, Model, Route, Method, Operation string
	} `json:"filter"`
}

// llmPreview returns, per bucket, the value an llm monitor compares at the bucket's end: window sums (cost, tokens,
// calls, errors; cost_usd_per_hour ÷ window hours), errors / calls over the window, or the bucket's p95 (merged
// t-digest). volume (minRequests) is the calls over the window for error_rate / p95 metrics, nil otherwise.
// Unpriced models count $0 in cost buckets.
func (d *DB) llmPreview(ctx context.Context, q Q, t model.MonitorTarget, raw json.RawMessage, cond model.MonitorCondition) (data, volume []float64, err error) {
	var pq llmPreviewQuery
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &pq); err != nil {
			return nil, nil, Invalid("query: " + err.Error())
		}
	}
	if err := validateLLMQuery(raw); err != nil {
		return nil, nil, err
	}
	f := pq.Filter
	if f.Service == "" {
		f.Service = t.Service
	}
	w, stepMin := q.W, q.W.StepMinutes()
	winMin := ParseWindow(cond.Window).Minutes()
	wh := (&where{}).eq("env", t.Env).eq("service", f.Service).eq("provider", f.Provider).eq("model", f.Model).
		eq("route", f.Route).eq("method", strings.ToUpper(f.Method)).eq("operation", f.Operation)
	from := ` FROM ` + llmTable(w) + ` WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)` + wh.sql
	args := append([]any{secs(w.From), secs(w.To)}, wh.args...)
	n := w.Points
	calls, errs, in, out, cost := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)

	rows, err := d.ch.Query(ctx, `SELECT provider, model, `+w.BucketExpr("minute")+` AS b, `+llmSumCols+from+` GROUP BY provider, model, b`, args...)
	if err != nil {
		return nil, nil, err
	}
	var prices *llmprice.Table
	if pq.Metric == "cost_usd" || pq.Metric == "cost_usd_per_hour" {
		prices = d.prices(ctx)
	}
	for rows.Next() {
		var provider, mdl string
		var b int64
		var s llmSums
		if err := rows.Scan(&provider, &mdl, &b, &s.calls, &s.errors, &s.in, &s.out, &s.cr, &s.cw); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if b < 0 || int(b) >= n {
			continue
		}
		calls[b] += s.calls
		errs[b] += s.errors
		in[b] += s.in
		out[b] += s.out
		if prices != nil {
			if v, ok := prices.Cost(provider, mdl, s.usage()); ok {
				cost[b] += v
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	sum := func(xs []float64) []float64 { return WindowSum(xs, stepMin, winMin) }
	switch pq.Metric {
	case "cost_usd":
		return sum(cost), nil, nil
	case "cost_usd_per_hour":
		v := sum(cost)
		for i := range v {
			v[i] /= winMin / 60
		}
		return v, nil, nil
	case "tokens":
		tok := make([]float64, n)
		for i := range tok {
			tok[i] = in[i] + out[i]
		}
		return sum(tok), nil, nil
	case "input_tokens":
		return sum(in), nil, nil
	case "output_tokens":
		return sum(out), nil, nil
	case "calls":
		return sum(calls), nil, nil
	case "errors":
		return sum(errs), nil, nil
	case "error_rate":
		c, e := sum(calls), sum(errs)
		v := make([]float64, n)
		for i := range v {
			v[i] = div(e[i], c[i])
		}
		return v, c, nil
	}
	// p95_ms | ttft_p95_ms: per-bucket merged digest (same levels as the rollup state; index 1 = p95)
	col := "duration_hist"
	if pq.Metric == "ttft_p95_ms" {
		col = "ttft_hist"
	}
	v := make([]float64, n)
	qrows, err := d.ch.Query(ctx, `SELECT `+w.BucketExpr("minute")+` AS b, arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(0.5, 0.95, 0.99)(`+col+`))`+
		from+` GROUP BY b`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer qrows.Close()
	for qrows.Next() {
		var b int64
		var qs []float64
		if err := qrows.Scan(&b, &qs); err != nil {
			return nil, nil, err
		}
		if b >= 0 && int(b) < n {
			v[b] = qt(qs, 1)
		}
	}
	return v, sum(calls), qrows.Err()
}
