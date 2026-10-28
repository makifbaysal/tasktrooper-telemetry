package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
	"github.com/tasktrooper/telemetry/services/pkg/llmprice"
)

// AI / LLM monitoring (ADR-023, contracts §25): llm_1m / llm_1h rollups, LLM spans (attrs gen_ai.*) and llm_content.
// Cost is computed here from llmprice (defaults + Postgres llm_prices org overrides).

// AIFilter narrows the AI queries.
type AIFilter struct {
	Service, Provider, Model, Route string
	ErrorsOnly                      bool
	Limit                           int
}

func llmTable(w timerange.Window) string {
	if w.Hourly() {
		return "llm_1h"
	}
	return "llm_1m"
}

// rollupConds: env / service / provider / model on llm_1m|1h.
func (f AIFilter) rollupConds(env string) (string, []any) {
	where := ""
	var args []any
	for _, c := range []struct{ col, v string }{{"env", env}, {"service", f.Service}, {"provider", f.Provider}, {"model", f.Model}} {
		if c.v != "" {
			where += " AND " + c.col + " = ?"
			args = append(args, c.v)
		}
	}
	return where, args
}

// ---------------------------------------------------------------- prices

// PriceTable returns the default table + the org's overrides.
func (d *DB) PriceTable(ctx context.Context) (*llmprice.Table, error) {
	rows, err := d.pg.Query(ctx, `SELECT provider, model, input_per_mtok::float8, output_per_mtok::float8, cache_read_per_mtok::float8,
	    cache_write_per_mtok::float8 FROM llm_prices WHERE org_id = {{org}} ORDER BY provider, model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var org []llmprice.Price
	for rows.Next() {
		var p llmprice.Price
		if err := rows.Scan(&p.Provider, &p.Model, &p.InputPerMTok, &p.OutputPerMTok, &p.CacheReadPerMTok, &p.CacheWritePerMTok); err != nil {
			return nil, err
		}
		org = append(org, p)
	}
	return llmprice.New(org), rows.Err()
}

// prices is PriceTable that never fails (defaults only on error): cost is an estimate, not a reason to fail a page.
func (d *DB) prices(ctx context.Context) *llmprice.Table {
	t, err := d.PriceTable(ctx)
	if err != nil {
		return llmprice.New(nil)
	}
	return t
}

// ListLLMPrices is GET /ai/prices.
func (d *DB) ListLLMPrices(ctx context.Context) (model.AIPrices, error) {
	t, err := d.PriceTable(ctx)
	if err != nil {
		return model.AIPrices{}, err
	}
	return model.AIPrices{Prices: t.List(), Updated: llmprice.Updated()}, nil
}

func validPrice(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v < 1e6 }

// SetLLMPrice upserts an org override.
func (d *DB) SetLLMPrice(ctx context.Context, in model.AIPriceInput) (*llmprice.Price, error) {
	in.Provider, in.Model = strings.TrimSpace(in.Provider), strings.ToLower(strings.TrimSpace(in.Model))
	if in.Provider == "" || in.Model == "" || len(in.Provider) > 128 || len(in.Model) > 256 {
		return nil, Invalid("provider and model are required (≤ 128 / 256 chars)")
	}
	if !validPrice(in.InputPerMTok) || !validPrice(in.OutputPerMTok) ||
		in.CacheReadPerMTok != nil && !validPrice(*in.CacheReadPerMTok) || in.CacheWritePerMTok != nil && !validPrice(*in.CacheWritePerMTok) {
		return nil, Invalid("prices are USD per 1M tokens: 0 ≤ price < 1e6")
	}
	var by *int64
	if a := ActorOf(ctx); a.UserID != 0 {
		by = &a.UserID
	}
	_, err := d.pg.Exec(ctx, `INSERT INTO llm_prices (org_id, provider, model, input_per_mtok, output_per_mtok, cache_read_per_mtok,
	    cache_write_per_mtok, updated_by, updated_at) VALUES ({{org}}, $1, $2, $3, $4, $5, $6, $7, now())
	  ON CONFLICT (org_id, provider, model) DO UPDATE SET input_per_mtok = excluded.input_per_mtok, output_per_mtok = excluded.output_per_mtok,
	    cache_read_per_mtok = excluded.cache_read_per_mtok, cache_write_per_mtok = excluded.cache_write_per_mtok,
	    updated_by = excluded.updated_by, updated_at = now()`,
		in.Provider, in.Model, in.InputPerMTok, in.OutputPerMTok, in.CacheReadPerMTok, in.CacheWritePerMTok, by)
	if err != nil {
		return nil, err
	}
	return &llmprice.Price{Provider: in.Provider, Model: in.Model, InputPerMTok: in.InputPerMTok, OutputPerMTok: in.OutputPerMTok,
		CacheReadPerMTok: in.CacheReadPerMTok, CacheWritePerMTok: in.CacheWritePerMTok, Source: "org"}, nil
}

// DeleteLLMPrice removes an org override (the default applies again).
func (d *DB) DeleteLLMPrice(ctx context.Context, provider, mdl string) error {
	provider, mdl = strings.TrimSpace(provider), strings.ToLower(strings.TrimSpace(mdl))
	if provider == "" || mdl == "" {
		return Invalid("provider and model are required")
	}
	tag, err := d.pg.Exec(ctx, `DELETE FROM llm_prices WHERE org_id = {{org}} AND provider = $1 AND model = $2`, provider, mdl)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- overview

type llmSums struct{ calls, errors, in, out, cr, cw float64 }

func (s *llmSums) add(o llmSums) {
	s.calls += o.calls
	s.errors += o.errors
	s.in += o.in
	s.out += o.out
	s.cr += o.cr
	s.cw += o.cw
}

func (s llmSums) usage() llmprice.Usage {
	return llmprice.Usage{Input: s.in, Output: s.out, CacheRead: s.cr, CacheWrite: s.cw}
}

const llmSumCols = `toFloat64(sum(calls)), toFloat64(sum(errors)), toFloat64(sum(input_tokens)), toFloat64(sum(output_tokens)),
    toFloat64(sum(cache_read_tokens)), toFloat64(sum(cache_write_tokens))`

// costAcc sums cost of priced rows and remembers whether some calls were unpriced.
type costAcc struct {
	usd     float64
	priced  bool
	partial bool
}

func (c *costAcc) add(t *llmprice.Table, provider, mdl string, s llmSums) {
	if v, ok := t.Cost(provider, mdl, s.usage()); ok {
		c.usd += v
		c.priced = true
	} else if s.calls > 0 {
		c.partial = true
	}
}

func (c costAcc) ptr() *float64 {
	if !c.priced {
		return nil
	}
	v := c.usd
	return &v
}

// AIOverview is GET /ai/overview.
func (d *DB) AIOverview(ctx context.Context, q Q, f AIFilter) (*model.AIOverview, error) {
	tbl := llmTable(q.W)
	cond, ca := f.rollupConds(q.Env)
	win := []any{secs(q.W.From), secs(q.W.To)}
	where := ` FROM ` + tbl + ` WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)` + cond
	args := append(win, ca...)
	prices := d.prices(ctx)
	out := &model.AIOverview{ByModel: []model.AIModelRow{}, ByService: []model.AIServiceRow{}, ByEndpoint: []model.AIEndpointRow{}}

	// per model × bucket: totals, per-model rows, calls / tokens / cost series
	type mkey struct{ provider, model string }
	rows, err := d.ch.Query(ctx, `SELECT provider, model, `+q.W.BucketExpr("minute")+` AS b, `+llmSumCols+where+` GROUP BY provider, model, b`, args...)
	if err != nil {
		return nil, err
	}
	byModel := map[mkey]*llmSums{}
	type bucketCost struct {
		k   mkey
		b   int
		usd float64
	}
	var costs []bucketCost
	calls := timerange.NewBuilder(q.W, "calls", "errors")
	tokens := timerange.NewBuilder(q.W, "input", "output")
	calls.Label("calls", "Calls")
	calls.Label("errors", "Errors")
	tokens.Label("input", "Input tokens")
	tokens.Label("output", "Output tokens")
	for rows.Next() {
		var k mkey
		var b int64
		var s llmSums
		if err := rows.Scan(&k.provider, &k.model, &b, &s.calls, &s.errors, &s.in, &s.out, &s.cr, &s.cw); err != nil {
			rows.Close()
			return nil, err
		}
		if byModel[k] == nil {
			byModel[k] = &llmSums{}
		}
		byModel[k].add(s)
		calls.Add("calls", int(b), s.calls)
		calls.Add("errors", int(b), s.errors)
		tokens.Add("input", int(b), s.in)
		tokens.Add("output", int(b), s.out)
		if v, ok := prices.Cost(k.provider, k.model, s.usage()); ok {
			costs = append(costs, bucketCost{k, int(b), v})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// p95 per model
	qrows, err := d.ch.Query(ctx, `SELECT provider, model, arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(0.5, 0.95, 0.99)(duration_hist))`+
		where+` GROUP BY provider, model`, args...)
	if err != nil {
		return nil, err
	}
	p95 := map[mkey]float64{}
	for qrows.Next() {
		var k mkey
		var qs []float64
		if err := qrows.Scan(&k.provider, &k.model, &qs); err != nil {
			qrows.Close()
			return nil, err
		}
		p95[k] = qt(qs, 1)
	}
	qrows.Close()
	if err := qrows.Err(); err != nil {
		return nil, err
	}

	var tot llmSums
	var totCost costAcc
	modelCost := map[mkey]float64{}
	for k, s := range byModel {
		tot.add(*s)
		var c costAcc
		c.add(prices, k.provider, k.model, *s)
		totCost.add(prices, k.provider, k.model, *s)
		if c.priced {
			modelCost[k] = c.usd
		}
		out.ByModel = append(out.ByModel, model.AIModelRow{Provider: k.provider, Model: k.model, Calls: s.calls, Errors: s.errors,
			InputTokens: s.in, OutputTokens: s.out, CostUsd: c.ptr(), P95Ms: p95[k], PriceKnown: c.priced})
	}
	sort.Slice(out.ByModel, func(i, j int) bool {
		if out.ByModel[i].Calls != out.ByModel[j].Calls {
			return out.ByModel[i].Calls > out.ByModel[j].Calls
		}
		return out.ByModel[i].Model < out.ByModel[j].Model
	})
	out.Totals = model.AITotals{Calls: tot.calls, Errors: tot.errors, ErrorRate: div(tot.errors, tot.calls), InputTokens: tot.in,
		OutputTokens: tot.out, CacheReadTokens: tot.cr, CostUsd: totCost.ptr(), CostPartial: totCost.partial}

	// cost series: top 8 models by cost, the rest "other"
	ranked := make([]mkey, 0, len(modelCost))
	for k := range modelCost {
		ranked = append(ranked, k)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if modelCost[ranked[i]] != modelCost[ranked[j]] {
			return modelCost[ranked[i]] > modelCost[ranked[j]]
		}
		return ranked[i].model < ranked[j].model
	})
	seriesID := map[mkey]string{}
	cost := timerange.NewBuilder(q.W)
	for i, k := range ranked {
		if i >= 8 {
			seriesID[k] = "other"
			continue
		}
		seriesID[k] = k.provider + "/" + k.model
		cost.Label(seriesID[k], k.model)
	}
	if len(ranked) > 8 {
		cost.Label("other", "Other")
	}
	for _, c := range costs {
		cost.Add(seriesID[c.k], c.b, c.usd)
	}

	// latency: overall + per bucket
	var oq, tq []float64
	if err := d.ch.QueryRow(ctx, `SELECT arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(0.5, 0.95, 0.99)(duration_hist)),
	    arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(0.5, 0.95, 0.99)(ttft_hist))`+where, args...).Scan(&oq, &tq); err != nil {
		return nil, err
	}
	out.Totals.P50Ms, out.Totals.P95Ms = qt(oq, 0), qt(oq, 1)
	if v := qt(tq, 1); v > 0 {
		out.Totals.TtftP95Ms = &v
	}
	lat := timerange.NewBuilder(q.W, "p95")
	lat.Label("p95", "p95 (ms)")
	lrows, err := d.ch.Query(ctx, `SELECT `+q.W.BucketExpr("minute")+` AS b, arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(0.5, 0.95, 0.99)(duration_hist))`+
		where+` GROUP BY b`, args...)
	if err != nil {
		return nil, err
	}
	for lrows.Next() {
		var b int64
		var qs []float64
		if err := lrows.Scan(&b, &qs); err != nil {
			lrows.Close()
			return nil, err
		}
		lat.Set("p95", int(b), qt(qs, 1))
	}
	lrows.Close()
	if err := lrows.Err(); err != nil {
		return nil, err
	}
	out.Series = model.AISeries{Calls: calls.Build(), Tokens: tokens.Build(), Cost: cost.Build(), Latency: lat.Build()}

	// services and endpoints
	if err := d.aiBreakdown(ctx, q, where, args, prices, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (d *DB) aiBreakdown(ctx context.Context, q Q, where string, args []any, prices *llmprice.Table, out *model.AIOverview) error {
	type ekey struct{ service, route, method string }
	type svcAcc struct {
		s      llmSums
		cost   costAcc
		models map[string]bool
	}
	type epAcc struct {
		s    llmSums
		cost costAcc
	}
	rows, err := d.ch.Query(ctx, `SELECT service, route, method, provider, model, `+llmSumCols+where+` GROUP BY service, route, method, provider, model`, args...)
	if err != nil {
		return err
	}
	svcs := map[string]*svcAcc{}
	eps := map[ekey]*epAcc{}
	for rows.Next() {
		var k ekey
		var provider, mdl string
		var s llmSums
		if err := rows.Scan(&k.service, &k.route, &k.method, &provider, &mdl, &s.calls, &s.errors, &s.in, &s.out, &s.cr, &s.cw); err != nil {
			rows.Close()
			return err
		}
		sa := svcs[k.service]
		if sa == nil {
			sa = &svcAcc{models: map[string]bool{}}
			svcs[k.service] = sa
		}
		sa.s.add(s)
		sa.cost.add(prices, provider, mdl, s)
		sa.models[mdl] = true
		ea := eps[k]
		if ea == nil {
			ea = &epAcc{}
			eps[k] = ea
		}
		ea.s.add(s)
		ea.cost.add(prices, provider, mdl, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for name, a := range svcs {
		models := make([]string, 0, len(a.models))
		for m := range a.models {
			if m != "" {
				models = append(models, m)
			}
		}
		sort.Strings(models)
		out.ByService = append(out.ByService, model.AIServiceRow{Service: name, Calls: a.s.calls, InputTokens: a.s.in, OutputTokens: a.s.out,
			CostUsd: a.cost.ptr(), Models: models})
	}
	sort.Slice(out.ByService, func(i, j int) bool {
		if out.ByService[i].Calls != out.ByService[j].Calls {
			return out.ByService[i].Calls > out.ByService[j].Calls
		}
		return out.ByService[i].Service < out.ByService[j].Service
	})
	keys := make([]ekey, 0, len(eps))
	for k := range eps {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if eps[keys[i]].s.calls != eps[keys[j]].s.calls {
			return eps[keys[i]].s.calls > eps[keys[j]].s.calls
		}
		return keys[i].service+keys[i].route+keys[i].method < keys[j].service+keys[j].route+keys[j].method
	})
	if len(keys) > 100 {
		keys = keys[:100]
	}
	if len(keys) == 0 {
		return nil
	}
	prows, err := d.ch.Query(ctx, `SELECT service, route, method, arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(0.5, 0.95, 0.99)(duration_hist))`+
		where+` GROUP BY service, route, method`, args...)
	if err != nil {
		return err
	}
	p95 := map[ekey]float64{}
	for prows.Next() {
		var k ekey
		var qs []float64
		if err := prows.Scan(&k.service, &k.route, &k.method, &qs); err != nil {
			prows.Close()
			return err
		}
		p95[k] = qt(qs, 1)
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return err
	}
	for _, k := range keys {
		a := eps[k]
		row := model.AIEndpointRow{Service: k.service, Route: k.route, Method: k.method, Calls: a.s.calls,
			AvgInputTokens: div(a.s.in, a.s.calls), AvgOutputTokens: div(a.s.out, a.s.calls), CostUsd: a.cost.ptr(), P95Ms: p95[k]}
		if row.CostUsd != nil && a.s.calls > 0 {
			v := *row.CostUsd / a.s.calls
			row.CostPerCallUsd = &v
		}
		out.ByEndpoint = append(out.ByEndpoint, row)
	}
	return nil
}

// ---------------------------------------------------------------- calls (spans)

// llmSpanCols reads an LLM span; gen_ai.provider.name falls back to the deprecated gen_ai.system.
const llmSpanCols = `toInt64(toUnixTimestamp64Milli(ts)), lower(hex(trace_id)), lower(hex(span_id)), service,
    if(entry_route != '', entry_route, route),
    if(attrs['gen_ai.provider.name'] != '', attrs['gen_ai.provider.name'], attrs['gen_ai.system']),
    if(attrs['gen_ai.response.model'] != '', attrs['gen_ai.response.model'], attrs['gen_ai.request.model']),
    attrs['gen_ai.operation.name'],
    toFloat64OrZero(attrs['gen_ai.usage.input_tokens']), toFloat64OrZero(attrs['gen_ai.usage.output_tokens']),
    toFloat64OrZero(attrs['gen_ai.usage.cache_read.input_tokens']), toFloat64OrZero(attrs['gen_ai.usage.cache_creation.input_tokens']),
    duration_ns / 1e6, attrs['trooper.llm.ttft_ms'], is_error, toString(error_type), attrs['error.type'], attrs['gen_ai.response.finish_reasons']`

type scanner interface{ Scan(...any) error }

func scanLLMCall(r scanner, prices *llmprice.Table) (model.AICall, error) {
	var c model.AICall
	var cr, cw float64
	var ttft, errType, errAttr, finish string
	var isErr uint8
	if err := r.Scan(&c.Ts, &c.TraceID, &c.SpanID, &c.Service, &c.Route, &c.Provider, &c.Model, &c.Operation, &c.InputTokens, &c.OutputTokens,
		&cr, &cw, &c.DurationMs, &ttft, &isErr, &errType, &errAttr, &finish); err != nil {
		return c, err
	}
	if v, err := strconv.ParseFloat(ttft, 64); err == nil && v > 0 {
		c.TtftMs = &v
	}
	if isErr == 1 {
		c.Error = firstNonEmpty(errType, errAttr, "error")
	}
	c.FinishReason = FinishReason(finish)
	if v, ok := prices.Cost(c.Provider, c.Model, llmprice.Usage{Input: c.InputTokens, Output: c.OutputTokens, CacheRead: cr, CacheWrite: cw}); ok {
		c.CostUsd = &v
	}
	return c, nil
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// FinishReason returns the first finish reason of gen_ai.response.finish_reasons (a JSON array, "a,b" or a plain string).
func FinishReason(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		var xs []string
		if json.Unmarshal([]byte(s), &xs) == nil && len(xs) > 0 {
			return xs[0]
		}
		s = strings.Trim(s, "[]")
	}
	s, _, _ = strings.Cut(s, ",")
	return strings.Trim(strings.TrimSpace(s), `"'`)
}

// AICalls returns the newest LLM spans.
func (d *DB) AICalls(ctx context.Context, q Q, f AIFilter) (model.AICalls, error) {
	where := ""
	var args []any
	if f.Service != "" {
		where += " AND service = ?"
		args = append(args, f.Service)
	}
	if f.Provider != "" {
		where += " AND if(attrs['gen_ai.provider.name'] != '', attrs['gen_ai.provider.name'], attrs['gen_ai.system']) = ?"
		args = append(args, f.Provider)
	}
	if f.Model != "" {
		where += " AND if(attrs['gen_ai.response.model'] != '', attrs['gen_ai.response.model'], attrs['gen_ai.request.model']) = ?"
		args = append(args, f.Model)
	}
	if f.Route != "" {
		where += " AND (entry_route = ? OR route = ?)"
		args = append(args, f.Route, f.Route)
	}
	if f.ErrorsOnly {
		where += " AND is_error = 1"
	}
	ec, ea := envCond(q.Env, "env")
	rows, err := d.ch.Query(ctx, `SELECT `+llmSpanCols+` FROM spans
	  WHERE {{tenant}} AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?) AND attrs['gen_ai.operation.name'] NOT IN ('', 'execute_tool', 'invoke_agent', 'create_agent')`+ec+where+`
	  ORDER BY ts DESC LIMIT ?`, append(append(append([]any{micros(q.W.From), micros(q.W.To)}, ea...), args...), clampLimit(f.Limit, 50, 500))...)
	if err != nil {
		return model.AICalls{}, err
	}
	prices := d.prices(ctx)
	out := model.AICalls{Calls: []model.AICall{}}
	traces := map[string]bool{}
	for rows.Next() {
		c, err := scanLLMCall(rows, prices)
		if err != nil {
			rows.Close()
			return out, err
		}
		traces[c.TraceID] = true
		out.Calls = append(out.Calls, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	content := d.llmContentSpans(ctx, keys(traces), q.W.From.Add(-time.Minute), q.W.To.Add(time.Minute))
	for i := range out.Calls {
		out.Calls[i].HasContent = content[out.Calls[i].TraceID+"/"+out.Calls[i].SpanID]
	}
	return out, nil
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// llmContentSpans returns "trace/span" of the spans with captured content (best effort).
func (d *DB) llmContentSpans(ctx context.Context, traces []string, from, to time.Time) map[string]bool {
	out := map[string]bool{}
	if len(traces) == 0 {
		return out
	}
	rows, err := d.ch.Query(ctx, `SELECT lower(hex(trace_id)), lower(hex(span_id)) FROM llm_content
	  WHERE {{tenant}} AND lower(hex(trace_id)) IN (?) AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?)`,
		traces, micros(from), micros(to))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t, s string
		if rows.Scan(&t, &s) == nil {
			out[t+"/"+s] = true
		}
	}
	return out
}

// ValidSpanID reports whether s is a 16-char hex span id.
func ValidSpanID(s string) bool {
	return len(s) == 16 && strings.Trim(strings.ToLower(s), "0123456789abcdef") == ""
}

// AICall returns one LLM span with its attributes and captured content.
func (d *DB) AICall(ctx context.Context, traceID, spanID string) (*model.AICallDetail, error) {
	traceID, spanID = strings.ToLower(traceID), strings.ToLower(spanID)
	if !ValidTraceID(traceID) || !ValidSpanID(spanID) {
		return nil, Invalid("trace id must be 32 and span id 16 hex chars")
	}
	var start, finish time.Time
	var spans float64
	if err := d.ch.QueryRow(ctx, `SELECT min(start), max(finish), toFloat64(sum(spans)) FROM trace_index WHERE {{tenant}} AND trace_id = unhex(?)`,
		traceID).Scan(&start, &finish, &spans); err != nil {
		return nil, err
	}
	if spans == 0 {
		return nil, ErrNotFound
	}
	rows, err := d.ch.Query(ctx, `SELECT `+llmSpanCols+`, attrs FROM spans WHERE {{tenant}} AND trace_id = unhex(?) AND span_id = unhex(?)
	  AND ts >= fromUnixTimestamp64Micro(?) AND ts <= fromUnixTimestamp64Micro(?) AND attrs['gen_ai.operation.name'] NOT IN ('', 'execute_tool', 'invoke_agent', 'create_agent') LIMIT 1`,
		traceID, spanID, micros(start)-1, micros(finish)+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	out := &model.AICallDetail{}
	var attrs map[string]string
	prices := d.prices(ctx)
	call, err := scanLLMCall(scanFunc(func(dest ...any) error { return rows.Scan(append(dest, &attrs)...) }), prices)
	if err != nil {
		return nil, err
	}
	out.Call, out.Attributes = call, attrs
	if out.Attributes == nil {
		out.Attributes = map[string]string{}
	}
	var c model.AIContent
	err = d.ch.QueryRow(ctx, `SELECT system, input, output FROM llm_content WHERE {{tenant}} AND trace_id = unhex(?) AND span_id = unhex(?) LIMIT 1`,
		traceID, spanID).Scan(&c.System, &c.Input, &c.Output)
	switch {
	case err == nil:
		out.Content = &c
		out.Call.HasContent = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		return nil, err
	}
	return out, nil
}

type scanFunc func(dest ...any) error

func (f scanFunc) Scan(dest ...any) error { return f(dest...) }

// ---------------------------------------------------------------- traces and services

// IsLLMCallOp reports whether a GenAI operation is a model call. Tool and agent spans carry
// gen_ai.operation.name too but are not LLM calls (contracts §25).
func IsLLMCallOp(op string) bool {
	switch op {
	case "", "execute_tool", "invoke_agent", "create_agent":
		return false
	}
	return true
}

// LLMFromAttrs builds the GenAI summary of a span (nil when it is not an LLM span).
func LLMFromAttrs(attrs map[string]string, prices *llmprice.Table) *model.SpanLLM {
	op := attrs["gen_ai.operation.name"]
	if !IsLLMCallOp(op) {
		return nil
	}
	l := &model.SpanLLM{Provider: firstNonEmpty(attrs["gen_ai.provider.name"], attrs["gen_ai.system"]),
		Model: firstNonEmpty(attrs["gen_ai.response.model"], attrs["gen_ai.request.model"]), Operation: op}
	num := func(k string) float64 {
		v, _ := strconv.ParseFloat(attrs[k], 64)
		return fin(v)
	}
	l.InputTokens, l.OutputTokens = num("gen_ai.usage.input_tokens"), num("gen_ai.usage.output_tokens")
	if v := num("trooper.llm.ttft_ms"); v > 0 {
		l.TtftMs = &v
	}
	if prices != nil {
		if v, ok := prices.Cost(l.Provider, l.Model, llmprice.Usage{Input: l.InputTokens, Output: l.OutputTokens,
			CacheRead: num("gen_ai.usage.cache_read.input_tokens"), CacheWrite: num("gen_ai.usage.cache_creation.input_tokens")}); ok {
			l.CostUsd = &v
		}
	}
	return l
}

// annotateLLM sets Span.LLM on the GenAI spans of a trace (best effort for prices / content).
func (d *DB) annotateLLM(ctx context.Context, t *model.Trace, start, finish time.Time) {
	var prices *llmprice.Table
	found := false
	for i := range t.Spans {
		if !IsLLMCallOp(t.Spans[i].Attrs["gen_ai.operation.name"]) {
			continue
		}
		if prices == nil {
			prices = d.prices(ctx)
		}
		t.Spans[i].LLM = LLMFromAttrs(t.Spans[i].Attrs, prices)
		found = true
	}
	if !found {
		return
	}
	content := d.llmContentSpans(ctx, []string{t.ID}, start.Add(-time.Second), finish.Add(time.Second))
	for i := range t.Spans {
		if t.Spans[i].LLM != nil {
			t.Spans[i].LLM.HasContent = content[t.ID+"/"+t.Spans[i].SpanID]
		}
	}
}

// ServiceHasAI reports whether a service made LLM calls in the last 7 days (best effort: false on error).
func (d *DB) ServiceHasAI(ctx context.Context, q Q, service string) bool {
	var n uint64
	err := d.ch.QueryRow(ctx, `SELECT count() FROM llm_1h WHERE {{tenant}} AND service = ? AND minute >= toDateTime(?)`,
		service, secs(q.Now.Add(-7*24*time.Hour))).Scan(&n)
	return err == nil && n > 0
}
