package eval

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
	"github.com/tasktrooper/telemetry/services/pkg/llmprice"
)

// Monitor kind `llm` (ADR-023, contracts §25): query {filter: {service?, provider?, model?, route?, method?, operation?},
// metric} over llm_1m (llm_1h for the whole hours of windows ≥ 6 h, llm_1m for the edges). Cost uses the shared price
// table (services/pkg/llmprice: embedded defaults + the org's llm_prices rows); calls of unpriced models are left out
// of cost_* and a filter whose calls are all unpriced is no_data.

// LLMMetrics are the metrics of kind llm.
var LLMMetrics = []string{"cost_usd", "cost_usd_per_hour", "tokens", "input_tokens", "output_tokens", "calls", "errors", "error_rate", "p95_ms", "ttft_p95_ms"}

// LLMHourlyFrom: windows at least this long read whole hours from llm_1h.
const LLMHourlyFrom = 6 * time.Hour

// PriceTables returns an org's price table (never nil; on error the defaults / last good table plus the error).
type PriceTables interface {
	Table(ctx context.Context, orgID int64) (*llmprice.Table, error)
}

// LLMGroup is one service × provider × model aggregate over the window.
type LLMGroup struct {
	Service, Provider, Model               string
	Calls, Errors, In, Out, CacheR, CacheW float64
	P95, TTFTP95                           float64 // NaN = none
}

// LLMAgg is the window aggregate: groups plus the overall (merged) quantiles.
type LLMAgg struct {
	Groups       []LLMGroup
	P95, TTFTP95 float64 // NaN = none / not queried
}

// LLMContributor is one group's share of the monitor value.
type LLMContributor struct {
	Service, Provider, Model string
	Value, Share, Calls      float64
}

// LLMOutcome is the evaluated value.
type LLMOutcome struct {
	Value          float64
	OK             bool // false → no_data
	Calls, Errors  float64
	Contributors   []LLMContributor // sorted, worst first
	UnpricedModels []string         // cost metrics: models without a price (excluded)
}

func validLLMMetric(m string) bool {
	for _, x := range LLMMetrics {
		if x == m {
			return true
		}
	}
	return false
}

func isCostMetric(m string) bool { return m == "cost_usd" || m == "cost_usd_per_hour" }

// llmRateLike: minRequests applies (contracts §25).
func llmRateLike(m string) bool { return m == "error_rate" || m == "p95_ms" || m == "ttft_p95_ms" }

func usable(v float64) bool { return fin(v) && v > 0 }

// LLMValue computes metric from the window aggregate. window is the evaluated length (cost per hour).
func LLMValue(metric string, a LLMAgg, prices *llmprice.Table, window time.Duration) LLMOutcome {
	var o LLMOutcome
	for _, g := range a.Groups {
		o.Calls += g.Calls
		o.Errors += g.Errors
	}
	contrib := make([]LLMContributor, 0, len(a.Groups))
	add := func(g LLMGroup, v float64) {
		contrib = append(contrib, LLMContributor{Service: g.Service, Provider: g.Provider, Model: g.Model, Value: v, Calls: g.Calls})
	}
	hours := window.Hours()
	switch metric {
	case "cost_usd", "cost_usd_per_hour":
		unpriced := map[string]bool{}
		priced := false
		for _, g := range a.Groups {
			c, ok := prices.Cost(g.Provider, g.Model, llmprice.Usage{Input: g.In, Output: g.Out, CacheRead: g.CacheR, CacheWrite: g.CacheW})
			if !ok {
				if g.Calls > 0 {
					unpriced[g.Model] = true
				}
				continue
			}
			priced = true
			if metric == "cost_usd_per_hour" && hours > 0 {
				c /= hours
			}
			o.Value += c
			add(g, c)
		}
		for m := range unpriced {
			o.UnpricedModels = append(o.UnpricedModels, m)
		}
		sort.Strings(o.UnpricedModels)
		// no calls at all → $0 (nothing spent); calls but none priced → unknown
		o.OK = priced || o.Calls == 0
	case "tokens", "input_tokens", "output_tokens", "calls", "errors":
		for _, g := range a.Groups {
			v := map[string]float64{"tokens": g.In + g.Out, "input_tokens": g.In, "output_tokens": g.Out, "calls": g.Calls, "errors": g.Errors}[metric]
			o.Value += v
			add(g, v)
		}
		o.OK = true
	case "error_rate":
		if o.Calls <= 0 {
			return o
		}
		o.Value, o.OK = o.Errors/o.Calls, true
		for _, g := range a.Groups {
			add(g, g.Errors) // share of the errors
		}
	case "p95_ms", "ttft_p95_ms":
		v := a.P95
		if metric == "ttft_p95_ms" {
			v = a.TTFTP95
		}
		if o.Calls <= 0 || !usable(v) {
			return o
		}
		o.Value, o.OK = v, true
		for _, g := range a.Groups {
			gv := g.P95
			if metric == "ttft_p95_ms" {
				gv = g.TTFTP95
			}
			if usable(gv) {
				add(g, gv)
			}
		}
	default:
		return o
	}
	latency := metric == "p95_ms" || metric == "ttft_p95_ms"
	var total float64
	for _, c := range contrib {
		if latency {
			total += c.Calls
		} else {
			total += c.Value
		}
	}
	for i := range contrib {
		base := contrib[i].Value
		if latency {
			base = contrib[i].Calls
		}
		if total > 0 {
			contrib[i].Share = base / total
		}
	}
	sort.SliceStable(contrib, func(i, j int) bool {
		if contrib[i].Value != contrib[j].Value {
			return contrib[i].Value > contrib[j].Value
		}
		return contrib[i].Calls > contrib[j].Calls
	})
	// zero contributions (e.g. groups without errors) explain nothing
	for len(contrib) > 0 && contrib[len(contrib)-1].Value == 0 {
		contrib = contrib[:len(contrib)-1]
	}
	o.Contributors = contrib
	return o
}

// llmRollup groups contributors by one dimension (model or service); latency metrics keep the worst value.
func llmRollup(cs []LLMContributor, metric string, byModel bool, n int) []map[string]any {
	latency := metric == "p95_ms" || metric == "ttft_p95_ms"
	type acc struct {
		provider, key       string
		value, share, calls float64
	}
	idx := map[string]*acc{}
	var order []*acc
	for _, c := range cs {
		k := c.Service
		if byModel {
			k = c.Provider + "\x00" + c.Model
		}
		a := idx[k]
		if a == nil {
			a = &acc{provider: c.Provider, key: c.Service}
			if byModel {
				a.key = c.Model
			}
			idx[k] = a
			order = append(order, a)
		}
		if latency {
			a.value = math.Max(a.value, c.Value)
		} else {
			a.value += c.Value
		}
		a.share += c.Share
		a.calls += c.Calls
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].value > order[j].value })
	if len(order) > n {
		order = order[:n]
	}
	out := make([]map[string]any, len(order))
	for i, a := range order {
		d := map[string]any{"value": a.value, "share": a.share, "calls": a.calls}
		if byModel {
			d["provider"], d["model"] = a.provider, a.key
		} else {
			d["service"] = a.key
		}
		out[i] = d
	}
	return out
}

// LLMSummary is the one-line cause: "LLM cost $34.20/h above $20/h — claude-sonnet-4-5 in storefront-bff 81%".
func LLMSummary(metric, op string, threshold float64, window time.Duration, o LLMOutcome) string {
	labels := map[string]string{
		"cost_usd": "LLM cost", "cost_usd_per_hour": "LLM cost", "tokens": "LLM tokens", "input_tokens": "LLM input tokens",
		"output_tokens": "LLM output tokens", "calls": "LLM calls", "errors": "LLM errors", "error_rate": "LLM error rate",
		"p95_ms": "LLM p95", "ttft_p95_ms": "LLM time to first token p95",
	}
	dir := "above"
	if model.LowerIsWorse(op) {
		dir = "below"
	}
	s := fmt.Sprintf("%s %s", labels[metric], FormatLLM(metric, o.Value))
	switch metric {
	case "cost_usd", "tokens", "input_tokens", "output_tokens", "calls", "errors":
		s += " in " + fmtWindow(window)
	}
	s += fmt.Sprintf(" %s %s", dir, FormatLLM(metric, threshold))
	if len(o.Contributors) > 0 && dir == "above" {
		c := o.Contributors[0]
		who := c.Model
		if who == "" {
			who = c.Provider
		}
		if c.Service != "" {
			who += " in " + c.Service
		}
		switch metric {
		case "p95_ms", "ttft_p95_ms":
			s += fmt.Sprintf(" — %s p95 %s", who, FormatLLM(metric, c.Value))
		case "error_rate":
			s += fmt.Sprintf(" — %s %.0f%% of errors", who, c.Share*100)
		default:
			s += fmt.Sprintf(" — %s %.0f%%", who, c.Share*100)
		}
	}
	return s
}

// FormatLLM renders a metric value with its unit.
func FormatLLM(metric string, v float64) string {
	switch metric {
	case "cost_usd":
		return "$" + money(v)
	case "cost_usd_per_hour":
		return "$" + money(v) + "/h"
	case "error_rate":
		return trimFloat(v*100, 1) + "%"
	case "p95_ms", "ttft_p95_ms":
		if v >= 1000 {
			return trimFloat(v/1000, 1) + " s"
		}
		return trimFloat(v, 0) + " ms"
	}
	return compactNum(v)
}

func money(v float64) string {
	switch {
	case v >= 1000:
		return compactNum(v)
	case v >= 1 && v == math.Trunc(v):
		return fmt.Sprintf("%.0f", v)
	case v >= 0.01 || v == 0:
		return fmt.Sprintf("%.2f", v)
	}
	return fmt.Sprintf("%.4f", v)
}

func trimFloat(v float64, prec int) string {
	s := fmt.Sprintf("%.*f", prec, v)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func compactNum(v float64) string {
	a := math.Abs(v)
	switch {
	case a >= 1e9:
		return trimFloat(v/1e9, 1) + "B"
	case a >= 1e6:
		return trimFloat(v/1e6, 1) + "M"
	case a >= 1e3:
		return trimFloat(v/1e3, 1) + "k"
	}
	return trimFloat(v, 2)
}

func fmtWindow(d time.Duration) string {
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dm", int(math.Round(d.Minutes())))
}

// LLMFilter returns the effective filter (filter.service falls back to target.service).
func LLMFilter(m *model.Monitor) model.InfraFilter {
	f := m.Query.Filter
	if f.Service == "" {
		f.Service = m.Target.Service
	}
	return f
}

// llmSource returns the FROM source and its args for [from, to): llm_1m, or for windows ≥ 6 h the whole hours from
// llm_1h and the partial hours at the edges from llm_1m (same columns, so sums and digests merge across both).
func llmSource(m *model.Monitor, from, to time.Time, window time.Duration) (string, []any, string) {
	f := LLMFilter(m)
	_, _, _, env := m.Scope()
	w := tenantWhere(m).eq("env", env).eq("service", f.Service).eq("provider", f.Provider).eq("model", f.Model).
		eq("route", f.Route).eq("method", strings.ToUpper(f.Method)).eq("operation", f.Operation)
	const cols = `service, provider, model, calls, errors, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, duration_hist, ttft_hist`
	hFrom := from.Truncate(time.Hour)
	if hFrom.Before(from) {
		hFrom = hFrom.Add(time.Hour)
	}
	hTo := to.Truncate(time.Hour)
	if window < LLMHourlyFrom || !hFrom.Before(hTo) {
		args := append(append([]any(nil), w.args...), from.Unix(), to.Unix())
		return `(SELECT ` + cols + ` FROM telemetry.llm_1m WHERE ` + w.sql() + ` AND minute >= toDateTime(?) AND minute < toDateTime(?))`, args, "llm_1m"
	}
	args := append(append([]any(nil), w.args...), hFrom.Unix(), hTo.Unix())
	args = append(append(args, w.args...), from.Unix(), hFrom.Unix(), hTo.Unix(), to.Unix())
	return `(SELECT ` + cols + ` FROM telemetry.llm_1h WHERE ` + w.sql() + ` AND minute >= toDateTime(?) AND minute < toDateTime(?)
		UNION ALL SELECT ` + cols + ` FROM telemetry.llm_1m WHERE ` + w.sql() + `
		AND ((minute >= toDateTime(?) AND minute < toDateTime(?)) OR (minute >= toDateTime(?) AND minute < toDateTime(?))))`, args, "llm_1h+llm_1m"
}

// quantile levels of llm_1m.duration_hist / ttft_hist (deploy/clickhouse/005_replay_profiles_llm.sql): the merge must
// use the state's levels; index 1 = p95.
const llmLevels = "0.5, 0.95, 0.99"

func q95(qs []float64) float64 {
	if len(qs) < 2 {
		return math.NaN()
	}
	return qs[1]
}

func (e *Evaluator) llmAgg(ctx context.Context, m *model.Monitor, metric string, from, to time.Time, window time.Duration) (LLMAgg, string, error) {
	src, args, table := llmSource(m, from, to, window)
	a := LLMAgg{P95: math.NaN(), TTFTP95: math.NaN()}
	latency := metric == "p95_ms" || metric == "ttft_p95_ms"
	qsel := ""
	if latency {
		qsel = `, arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(` + llmLevels + `)(duration_hist)),
			arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(` + llmLevels + `)(ttft_hist))`
	}
	rows, err := e.CH.Query(ctx, `SELECT service, provider, model, toFloat64(sum(calls)), toFloat64(sum(errors)),
		toFloat64(sum(input_tokens)), toFloat64(sum(output_tokens)), toFloat64(sum(cache_read_tokens)), toFloat64(sum(cache_write_tokens))`+qsel+`
		FROM `+src+` GROUP BY service, provider, model`, args...)
	if err != nil {
		return a, table, err
	}
	for rows.Next() {
		g := LLMGroup{P95: math.NaN(), TTFTP95: math.NaN()}
		dest := []any{&g.Service, &g.Provider, &g.Model, &g.Calls, &g.Errors, &g.In, &g.Out, &g.CacheR, &g.CacheW}
		var dq, tq []float64
		if latency {
			dest = append(dest, &dq, &tq)
		}
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return a, table, err
		}
		g.P95, g.TTFTP95 = q95(dq), q95(tq)
		a.Groups = append(a.Groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return a, table, err
	}
	if latency && len(a.Groups) > 0 {
		var dq, tq []float64
		if err := e.CH.QueryRow(ctx, `SELECT arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(`+llmLevels+`)(duration_hist)),
			arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(`+llmLevels+`)(ttft_hist)) FROM `+src, args...).Scan(&dq, &tq); err != nil {
			return a, table, err
		}
		a.P95, a.TTFTP95 = q95(dq), q95(tq)
	}
	return a, table, nil
}

func (e *Evaluator) prices(ctx context.Context, org uint32) *llmprice.Table {
	if e.Prices == nil {
		return llmprice.New(nil)
	}
	t, _ := e.Prices.Table(ctx, int64(org)) // cost is an estimate: a failed override load falls back to the defaults
	if t == nil {
		return llmprice.New(nil)
	}
	return t
}

func (e *Evaluator) llm(ctx context.Context, m *model.Monitor, now time.Time) (Result, error) {
	metric := strings.ToLower(m.Query.Metric)
	if !validLLMMetric(metric) {
		return Result{NoData: true}, fmt.Errorf("%w: llm metric %q", ErrUnsupported, m.Query.Metric)
	}
	from, to := window(now, m.Condition.Window)
	win := to.Sub(from)
	a, table, err := e.llmAgg(ctx, m, metric, from, to, win)
	if err != nil {
		return Result{}, err
	}
	var prices *llmprice.Table
	if isCostMetric(metric) {
		prices = e.prices(ctx, m.OrgID)
	}
	o := LLMValue(metric, a, prices, win)
	if !o.OK {
		r := Result{NoData: true, Detail: map[string]any{"metric": metric, "calls": o.Calls}}
		if len(o.UnpricedModels) > 0 {
			r.Detail["unpricedModels"] = o.UnpricedModels
		}
		return r, nil
	}
	c := Condition(m)
	r := compare(m, o.Value)
	if llmRateLike(metric) && c.MinRequests > 0 && o.Calls < c.MinRequests {
		r.Breached = false
		r.Detail["lowTraffic"] = true
	}
	top := o.Contributors
	if len(top) > MaxDetailEntities {
		top = top[:MaxDetailEntities]
	}
	cs := make([]map[string]any, len(top))
	for i, t := range top {
		cs[i] = map[string]any{"service": t.Service, "provider": t.Provider, "model": t.Model, "value": t.Value, "share": t.Share, "calls": t.Calls}
	}
	r.Detail["metric"], r.Detail["window"], r.Detail["source"] = metric, fmtWindow(win), table
	r.Detail["calls"], r.Detail["errors"] = o.Calls, o.Errors
	r.Detail["contributors"] = cs
	r.Detail["topModels"] = llmRollup(o.Contributors, metric, true, MaxDetailEntities)
	r.Detail["topServices"] = llmRollup(o.Contributors, metric, false, MaxDetailEntities)
	if len(o.UnpricedModels) > 0 {
		r.Detail["unpricedModels"] = o.UnpricedModels
	}
	r.Detail["summary"] = LLMSummary(metric, c.Op, c.Threshold, win, o)
	if len(o.Contributors) > 0 {
		r.Detail["topShare"] = o.Contributors[0].Share
	}
	return r, nil
}
