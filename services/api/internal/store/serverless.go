package store

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

// Serverless functions (ADR-020, contracts §21): metadata from Postgres serverless_functions (trooper-cloud
// discovery), minute metrics from ClickHouse faas_1m (function_id = slug), cold starts and errors from spans.

// FaasPrices are the cost estimate constants (USD). Env overrides: TT_FAAS_LAMBDA_REQUEST_PRICE (per 1M requests),
// TT_FAAS_LAMBDA_GBS_PRICE, TT_FAAS_LAMBDA_GBS_PRICE_ARM64, TT_FAAS_AZURE_GBS_PRICE, TT_FAAS_AZURE_REQUEST_PRICE.
type FaasPrices struct {
	LambdaRequest, LambdaGBs, LambdaGBsArm, AzureGBs, AzureRequest float64
}

// DefaultFaasPrices are the list prices (free tier not deducted).
var DefaultFaasPrices = FaasPrices{LambdaRequest: 0.20, LambdaGBs: 0.0000166667, LambdaGBsArm: 0.0000133334, AzureGBs: 0.000016, AzureRequest: 0.20}

var (
	faasOnce   sync.Once
	faasPrices FaasPrices
)

func envFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(key)), 64); err == nil && v >= 0 && !math.IsInf(v, 0) {
		return v
	}
	return def
}

// FaasPricesFromEnv applies the env overrides to DefaultFaasPrices.
func FaasPricesFromEnv() FaasPrices {
	p := DefaultFaasPrices
	p.LambdaRequest = envFloat("TT_FAAS_LAMBDA_REQUEST_PRICE", p.LambdaRequest)
	p.LambdaGBs = envFloat("TT_FAAS_LAMBDA_GBS_PRICE", p.LambdaGBs)
	p.LambdaGBsArm = envFloat("TT_FAAS_LAMBDA_GBS_PRICE_ARM64", p.LambdaGBsArm)
	p.AzureGBs = envFloat("TT_FAAS_AZURE_GBS_PRICE", p.AzureGBs)
	p.AzureRequest = envFloat("TT_FAAS_AZURE_REQUEST_PRICE", p.AzureRequest)
	return p
}

func prices() FaasPrices {
	faasOnce.Do(func() { faasPrices = FaasPricesFromEnv() })
	return faasPrices
}

// faasTotals are a function's range totals.
type faasTotals struct {
	inv, errs, thr, invMs, conc, units float64
	p95, p99                           float64
}

// FaasCost estimates the range cost (USD): Lambda requests + GB-s (arm64 price for arm64), Azure execution units
// (MB·ms) + requests.
func FaasCost(p FaasPrices, provider, arch string, memoryMb *int64, t faasTotals) float64 {
	switch provider {
	case "aws":
		mem := 128.0
		if memoryMb != nil && *memoryMb > 0 {
			mem = float64(*memoryMb)
		}
		gbs := t.invMs / 1000 * mem / 1024
		price := p.LambdaGBs
		if strings.EqualFold(arch, "arm64") {
			price = p.LambdaGBsArm
		}
		return fin(t.inv*p.LambdaRequest/1e6 + gbs*price)
	case "azure":
		return fin(t.units/1024/1000*p.AzureGBs + t.inv*p.AzureRequest/1e6)
	}
	return 0
}

// FunctionStatus: missing (gone in discovery), idle (no invocations in the range), critical error rate > 0.05 or
// throttles, warning error rate > 0.01 or p95 > 80% of the timeout.
func FunctionStatus(missing bool, inv, errorRate, throttles, p95Ms float64, timeoutS *int64) string {
	switch {
	case missing:
		return "missing"
	case inv <= 0:
		return "idle"
	case errorRate > 0.05 || throttles > 0:
		return "critical"
	case errorRate > 0.01 || (timeoutS != nil && *timeoutS > 0 && p95Ms > float64(*timeoutS)*1000*0.8):
		return "warning"
	}
	return "ok"
}

type fnMeta struct {
	id      int64
	f       model.ServerlessFunction
	missing bool
	meta    map[string]any
	integ   int64
	integNm string
}

const fnSelect = `SELECT f.id, f.slug, f.name, f.provider, coalesce(f.app, ''), f.region, f.runtime, f.memory_mb::bigint, f.timeout_s::bigint, f.arch,
  f.triggers, f.service, f.meta, f.missing, coalesce(f.integration_id, 0), coalesce(i.name, '')
  FROM serverless_functions f LEFT JOIN integrations i ON i.id = f.integration_id AND i.project_id = f.project_id`

func (d *DB) fnMetas(ctx context.Context, where string, args ...any) ([]fnMeta, error) {
	rows, err := d.pg.Query(ctx, fnSelect+` WHERE {{tenant:f}}`+where+` ORDER BY f.provider, f.name, f.slug`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fnMeta
	for rows.Next() {
		var m fnMeta
		var meta []byte
		if err := rows.Scan(&m.id, &m.f.Slug, &m.f.Name, &m.f.Provider, &m.f.App, &m.f.Region, &m.f.Runtime, &m.f.MemoryMb, &m.f.TimeoutS,
			&m.f.Arch, &m.f.Triggers, &m.f.Service, &meta, &m.missing, &m.integ, &m.integNm); err != nil {
			return nil, err
		}
		m.f.ID = strconv.FormatInt(m.id, 10)
		if m.f.Triggers == nil {
			m.f.Triggers = []string{}
		}
		_ = json.Unmarshal(meta, &m.meta)
		out = append(out, m)
	}
	return out, rows.Err()
}

// faasTotals reads range totals per function_id (minute values summed; p95/p99 invocation-weighted).
func (d *DB) faasTotals(ctx context.Context, q Q, slugs []string) (map[string]faasTotals, error) {
	out := map[string]faasTotals{}
	if len(slugs) == 0 {
		return out, nil
	}
	rows, err := d.ch.Query(ctx, `SELECT f,
	    toFloat64(sumIf(inv, isFinite(inv))), toFloat64(sumIf(err, isFinite(err))), toFloat64(sumIf(thr, isFinite(thr))),
	    toFloat64(sumIf(inv * davg, isFinite(inv) AND isFinite(davg))),
	    toFloat64(maxIf(conc, isFinite(conc))), toFloat64(sumIf(eu, isFinite(eu))),
	    toFloat64(sumIf(inv * p95, isFinite(inv) AND isFinite(p95))), toFloat64(sumIf(inv, isFinite(inv) AND isFinite(p95))), toFloat64(maxIf(p95, isFinite(p95))),
	    toFloat64(sumIf(inv * p99, isFinite(inv) AND isFinite(p99))), toFloat64(sumIf(inv, isFinite(inv) AND isFinite(p99))), toFloat64(maxIf(p99, isFinite(p99)))
	  FROM (SELECT toString(function_id) AS f, minute AS m, max(invocations) AS inv, max(errors) AS err, max(throttles) AS thr,
	      max(duration_avg) AS davg, max(duration_p95) AS p95, max(duration_p99) AS p99, max(concurrency) AS conc, max(execution_units) AS eu
	    FROM faas_1m WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?) AND function_id IN (?) GROUP BY f, m)
	  GROUP BY f`, secs(q.W.From), secs(q.W.To), slugs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f string
		var t faasTotals
		var w95, n95, m95, w99, n99, m99 float64
		if err := rows.Scan(&f, &t.inv, &t.errs, &t.thr, &t.invMs, &t.conc, &t.units, &w95, &n95, &m95, &w99, &n99, &m99); err != nil {
			return nil, err
		}
		t.p95, t.p99 = fin(m95), fin(m99)
		if n95 > 0 {
			t.p95 = fin(w95 / n95)
		}
		if n99 > 0 {
			t.p99 = fin(w99 / n99)
		}
		t.inv, t.errs, t.thr, t.invMs, t.conc, t.units = fin(t.inv), fin(t.errs), fin(t.thr), fin(t.invMs), fin(t.conc), fin(t.units)
		out[f] = t
	}
	return out, rows.Err()
}

// coldStarts counts entry spans with faas.coldstart=true per (service, faas.name) in [from, to).
func (d *DB) coldStarts(ctx context.Context, q Q, services []string) (map[[2]string]float64, error) {
	out := map[[2]string]float64{}
	if len(services) == 0 {
		return out, nil
	}
	rows, err := d.ch.Query(ctx, `SELECT service, attrs['faas.name'], toFloat64(sum(sample_weight)) FROM spans
	  WHERE {{tenant}} AND ts >= toDateTime64(?, 6) AND ts < toDateTime64(?, 6) AND is_entry = 1 AND attrs['faas.coldstart'] = 'true'
	    AND service IN (?) GROUP BY 1, 2`, secs(q.W.From), secs(q.W.To), services)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s, n string
		var v float64
		if err := rows.Scan(&s, &n, &v); err != nil {
			return nil, err
		}
		out[[2]string{s, n}] += fin(v)
	}
	return out, rows.Err()
}

// coldFor attributes cold starts to fn: all of its service's when it is the only function of that service, else
// the spans whose faas.name is the function's name (or one of an Azure app's functions).
func coldFor(m fnMeta, cold map[[2]string]float64, perService map[string]int) float64 {
	var v float64
	names := map[string]bool{m.f.Name: true}
	for _, n := range metaFunctions(m.meta) {
		names[n] = true
	}
	for k, c := range cold {
		if k[0] != m.f.Service {
			continue
		}
		if perService[m.f.Service] <= 1 || names[k[1]] {
			v += c
		}
	}
	return v
}

func metaFunctions(meta map[string]any) []string {
	var out []string
	if xs, ok := meta["functions"].([]any); ok {
		for _, x := range xs {
			switch v := x.(type) {
			case string:
				out = append(out, v)
			case map[string]any:
				if n, _ := v["name"].(string); n != "" {
					out = append(out, n)
				}
			}
		}
	}
	return out
}

// ServerlessFilter narrows GET /serverless/functions.
type ServerlessFilter struct {
	Provider, Q string
}

func (d *DB) functions(ctx context.Context, q Q, metas []fnMeta) ([]model.ServerlessFunction, error) {
	slugs := make([]string, 0, len(metas))
	svcSet := map[string]bool{}
	perService := map[string]int{}
	for _, m := range metas {
		slugs = append(slugs, m.f.Slug)
		if m.f.Service != "" {
			svcSet[m.f.Service] = true
			perService[m.f.Service]++
		}
	}
	tot, err := d.faasTotals(ctx, q, slugs)
	if err != nil {
		return nil, err
	}
	var svcs []string
	for s := range svcSet {
		svcs = append(svcs, s)
	}
	sort.Strings(svcs)
	cold, err := d.coldStarts(ctx, q, svcs)
	if err != nil {
		return nil, err
	}
	var spark map[string][][]float64
	if len(slugs) > 0 {
		spark, err = d.gaugeBuckets(ctx, q.W, seriesQuery{table: "faas_1m", entity: []string{"function_id"}, group: "toString(function_id)",
			cols: []string{"invocations"}, where: " AND function_id IN (?)", args: []any{slugs}, perBucket: "sum"})
		if err != nil {
			return nil, err
		}
	}
	pr := prices()
	out := make([]model.ServerlessFunction, 0, len(metas))
	for _, m := range metas {
		f := m.f
		t := tot[f.Slug]
		f.Invocations, f.Errors, f.Throttles, f.P95Ms, f.P99Ms, f.ConcurrencyMax = t.inv, t.errs, t.thr, t.p95, t.p99, t.conc
		f.ErrorRate = div(t.errs, t.inv)
		f.ColdStarts = coldFor(m, cold, perService)
		f.ColdStartRate = math.Min(1, div(f.ColdStarts, t.inv))
		f.CostUSD = math.Round(FaasCost(pr, f.Provider, f.Arch, f.MemoryMb, t)*1e6) / 1e6
		f.Status = FunctionStatus(m.missing, t.inv, f.ErrorRate, t.thr, t.p95, f.TimeoutS)
		f.Spark = make([]float64, q.W.Points)
		if s := spark[f.Slug]; s != nil {
			f.Spark = s[0]
		}
		out = append(out, f)
	}
	return out, nil
}

// ListFunctions is GET /serverless/functions.
func (d *DB) ListFunctions(ctx context.Context, q Q, f ServerlessFilter) ([]model.ServerlessFunction, error) {
	where, args := "", []any{}
	if f.Provider != "" {
		args = append(args, f.Provider)
		where += " AND f.provider = $" + strconv.Itoa(len(args))
	}
	if s := strings.TrimSpace(f.Q); s != "" {
		args = append(args, "%"+strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(s)+"%")
		n := "$" + strconv.Itoa(len(args))
		where += " AND (f.name ILIKE " + n + " OR f.slug ILIKE " + n + " OR f.service ILIKE " + n + " OR coalesce(f.app, '') ILIKE " + n + ")"
	}
	metas, err := d.fnMetas(ctx, where, args...)
	if err != nil {
		return nil, err
	}
	out, err := d.functions(ctx, q, metas)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Invocations > out[j].Invocations })
	return out, nil
}

// ServerlessSummary is GET /serverless/summary.
func (d *DB) ServerlessSummary(ctx context.Context, q Q) (*model.ServerlessSummary, error) {
	fns, err := d.ListFunctions(ctx, q, ServerlessFilter{})
	if err != nil {
		return nil, err
	}
	s := &model.ServerlessSummary{Functions: len(fns), ByProvider: []model.ServerlessProvider{}}
	by := map[string]*model.ServerlessProvider{}
	for _, f := range fns {
		s.Invocations += f.Invocations
		s.Errors += f.Errors
		s.Throttles += f.Throttles
		s.ColdStarts += f.ColdStarts
		s.CostUSD += f.CostUSD
		p, ok := by[f.Provider]
		if !ok {
			p = &model.ServerlessProvider{Provider: f.Provider}
			by[f.Provider] = p
		}
		p.Functions++
		p.Invocations += f.Invocations
		p.CostUSD += f.CostUSD
	}
	s.ErrorRate = div(s.Errors, s.Invocations)
	s.CostUSD = math.Round(s.CostUSD*1e6) / 1e6
	for _, p := range by {
		p.CostUSD = math.Round(p.CostUSD*1e6) / 1e6
		s.ByProvider = append(s.ByProvider, *p)
	}
	sort.Slice(s.ByProvider, func(i, j int) bool { return s.ByProvider[i].Provider < s.ByProvider[j].Provider })
	return s, nil
}

// GetFunction is GET /serverless/functions/{id} (numeric id or slug).
func (d *DB) GetFunction(ctx context.Context, q Q, id string) (*model.ServerlessFunctionDetail, error) {
	var metas []fnMeta
	var err error
	if n, perr := strconv.ParseInt(id, 10, 64); perr == nil && n > 0 {
		metas, err = d.fnMetas(ctx, " AND f.id = $1", n)
	} else {
		metas, err = d.fnMetas(ctx, " AND f.slug = $1", id)
	}
	if err != nil {
		return nil, err
	}
	if len(metas) == 0 {
		return nil, ErrNotFound
	}
	m := metas[0]
	fns, err := d.functions(ctx, q, metas)
	if err != nil {
		return nil, err
	}
	out := &model.ServerlessFunctionDetail{ServerlessFunction: fns[0], RecentErrors: []model.FunctionError{}}
	where, args := " AND function_id = ?", []any{m.f.Slug}
	sums, err := d.gaugeBuckets(ctx, q.W, seriesQuery{table: "faas_1m", entity: []string{"function_id"}, cols: []string{"invocations", "errors", "throttles"},
		where: where, args: args, perBucket: "sum"})
	if err != nil {
		return nil, err
	}
	avgs, err := d.gaugeBuckets(ctx, q.W, seriesQuery{table: "faas_1m", entity: []string{"function_id"}, cols: []string{"duration_avg"}, where: where, args: args})
	if err != nil {
		return nil, err
	}
	maxs, err := d.gaugeBuckets(ctx, q.W, seriesQuery{table: "faas_1m", entity: []string{"function_id"}, cols: []string{"duration_p95", "duration_p99", "concurrency"},
		where: where, args: args, perBucket: "max"})
	if err != nil {
		return nil, err
	}
	g := func(x map[string][][]float64, n int) [][]float64 {
		if s := x[""]; s != nil {
			return s
		}
		return make([][]float64, n)
	}
	s, a, mx := g(sums, 3), g(avgs, 1), g(maxs, 3)
	out.Series.Invocations = set(q.W, []string{"invocations", "errors", "throttles"}, s)
	out.Series.Duration = set(q.W, []string{"avg", "p95", "p99"}, [][]float64{a[0], mx[0], mx[1]})
	out.Series.Concurrency = set(q.W, []string{"concurrency"}, mx[2:3])
	cold := make([]float64, q.W.Points)
	if m.f.Service != "" {
		nameCond, nargs := "", []any{}
		if others, err := d.fnMetas(ctx, " AND f.service = $1", m.f.Service); err == nil && len(others) > 1 {
			names := append([]string{m.f.Name}, metaFunctions(m.meta)...)
			nameCond, nargs = " AND attrs['faas.name'] IN (?)", []any{names}
		}
		rows, err := d.ch.Query(ctx, `SELECT toInt64(`+q.W.BucketExpr("ts")+`) AS b, toFloat64(sum(sample_weight)) FROM spans
		  WHERE {{tenant}} AND ts >= toDateTime64(?, 6) AND ts < toDateTime64(?, 6) AND is_entry = 1 AND attrs['faas.coldstart'] = 'true'
		    AND service = ?`+nameCond+` GROUP BY b`, append([]any{secs(q.W.From), secs(q.W.To), m.f.Service}, nargs...)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var b int64
			var v float64
			if err := rows.Scan(&b, &v); err != nil {
				rows.Close()
				return nil, err
			}
			if b >= 0 && int(b) < len(cold) {
				cold[b] = fin(v)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		rows, err = d.ch.Query(ctx, `SELECT toInt64(toUnixTimestamp64Milli(ts)), lower(hex(trace_id)),
		    coalesce(nullif(attrs['exception.message'], ''), nullif(toString(error_type), ''), if(status_code > 0, concat('HTTP ', toString(status_code)), name))
		  FROM spans WHERE {{tenant}} AND ts >= toDateTime64(?, 6) AND ts < toDateTime64(?, 6) AND is_entry = 1 AND is_error = 1 AND service = ?`+nameCond+`
		  ORDER BY ts DESC LIMIT 20`, append([]any{secs(q.W.From), secs(q.W.To), m.f.Service}, nargs...)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var e model.FunctionError
			if err := rows.Scan(&e.TS, &e.TraceID, &e.Message); err != nil {
				rows.Close()
				return nil, err
			}
			out.RecentErrors = append(out.RecentErrors, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	out.Series.ColdStarts = set(q.W, []string{"coldStarts"}, [][]float64{cold})
	if m.f.Provider == "azure" {
		out.Functions = metaFunctions(m.meta)
		if out.Functions == nil {
			out.Functions = []string{}
		}
	}
	if m.integ > 0 {
		out.Integration = &model.IntegrationRef{ID: strconv.FormatInt(m.integ, 10), Name: m.integNm}
	}
	return out, nil
}

// serviceFunctions lists the functions mapped to a span service (GET /services/{svc}/infra).
func (d *DB) serviceFunctions(ctx context.Context, svc string) ([]model.ServiceFunction, error) {
	rows, err := d.pg.Query(ctx, `SELECT id, name, provider FROM serverless_functions WHERE {{tenant}} AND service = $1 ORDER BY name`, svc)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ServiceFunction{}
	for rows.Next() {
		var id int64
		var f model.ServiceFunction
		if err := rows.Scan(&id, &f.Name, &f.Provider); err != nil {
			return nil, err
		}
		f.ID = strconv.FormatInt(id, 10)
		out = append(out, f)
	}
	return out, rows.Err()
}

// functionResources lists the functions an integration discovered (integration resources, kind 'function').
func (d *DB) functionResources(ctx context.Context, ids []int64) (map[int64][]model.DiscoveredResource, error) {
	out := map[int64][]model.DiscoveredResource{}
	rows, err := d.pg.Query(ctx, `SELECT integration_id, slug, name, concat_ws(' · ', nullif(region, ''), nullif(runtime, '')), missing
	  FROM serverless_functions WHERE {{tenant}} AND integration_id = ANY($1) ORDER BY integration_id, name`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var r model.DiscoveredResource
		var missing bool
		if err := rows.Scan(&id, &r.RefID, &r.Name, &r.Detail, &missing); err != nil {
			return nil, err
		}
		r.Kind = "function"
		if missing {
			r.Detail = strings.TrimPrefix(r.Detail+" · missing in the last discovery", " · ")
		}
		out[id] = append(out[id], r)
	}
	return out, rows.Err()
}
