package eval

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
)

// Monitor kind `faas` (contracts §21): faas_1m per function (minute values from CloudWatch / Azure Monitor),
// cold starts from spans (attrs['faas.coldstart'] = 'true', entry spans of the function's service). The monitor value
// is the worst function's value; detail.entities lists the worst 5.

// FunctionLookup resolves serverless_functions rows (Postgres). service and slug are optional filters.
type FunctionLookup interface {
	ServerlessFunctions(ctx context.Context, projectID int64, service, slug string) ([]model.Function, error)
}

// FaaSMetrics are the metrics of kind faas.
var FaaSMetrics = []string{"invocations", "errors", "error_rate", "throttles", "duration_p95", "duration_p99", "cold_starts", "concurrency", "iterator_age_ms"}

// FaaSMinute is one faas_1m minute of a function (non-finite = not reported).
type FaaSMinute struct {
	Invocations, Errors, Throttles, P95, P99, Concurrency, IteratorAge float64
}

// FaaSAgg is a function's window aggregate.
type FaaSAgg struct {
	Invocations, Errors, Throttles float64
	HasInv, HasErr, HasThr         bool
	P95, P99                       float64 // invocation-weighted mean of the minute values (NaN = none)
	Concurrency, IteratorAge       float64 // max (NaN = none)
}

func fin(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// AggregateFaaS folds minute values: counts are summed, p95/p99 averaged weighted by invocations (plain mean when no
// minute has invocations), concurrency / iterator age maxed.
func AggregateFaaS(mins []FaaSMinute) FaaSAgg {
	a := FaaSAgg{P95: math.NaN(), P99: math.NaN(), Concurrency: math.NaN(), IteratorAge: math.NaN()}
	wavg := func(get func(FaaSMinute) float64) float64 {
		var sw, swv, s float64
		n := 0
		for _, m := range mins {
			v := get(m)
			if !fin(v) {
				continue
			}
			s += v
			n++
			if fin(m.Invocations) && m.Invocations > 0 {
				sw += m.Invocations
				swv += m.Invocations * v
			}
		}
		switch {
		case sw > 0:
			return swv / sw
		case n > 0:
			return s / float64(n)
		}
		return math.NaN()
	}
	for _, m := range mins {
		if fin(m.Invocations) {
			a.Invocations += m.Invocations
			a.HasInv = true
		}
		if fin(m.Errors) {
			a.Errors += m.Errors
			a.HasErr = true
		}
		if fin(m.Throttles) {
			a.Throttles += m.Throttles
			a.HasThr = true
		}
		if fin(m.Concurrency) && !(m.Concurrency <= a.Concurrency) {
			a.Concurrency = m.Concurrency
		}
		if fin(m.IteratorAge) && !(m.IteratorAge <= a.IteratorAge) {
			a.IteratorAge = m.IteratorAge
		}
	}
	a.P95 = wavg(func(m FaaSMinute) float64 { return m.P95 })
	a.P99 = wavg(func(m FaaSMinute) float64 { return m.P99 })
	return a
}

// FaaSValue returns a function's metric value (ok = false: not reported in the window).
func FaaSValue(metric string, a FaaSAgg, timeoutS float64, relative bool) (float64, bool) {
	ratio := func(v float64) (float64, bool) {
		if !fin(v) {
			return 0, false
		}
		if relative {
			if timeoutS <= 0 {
				return 0, false
			}
			return v / (timeoutS * 1000), true
		}
		return v, true
	}
	switch metric {
	case "invocations":
		return a.Invocations, a.HasInv
	case "errors":
		return a.Errors, a.HasErr
	case "throttles":
		return a.Throttles, a.HasThr
	case "error_rate":
		if !a.HasInv || a.Invocations <= 0 {
			return 0, false
		}
		return a.Errors / a.Invocations, true
	case "duration_p95":
		return ratio(a.P95)
	case "duration_p99":
		return ratio(a.P99)
	case "concurrency":
		return a.Concurrency, fin(a.Concurrency)
	case "iterator_age_ms":
		return a.IteratorAge, fin(a.IteratorAge)
	}
	return 0, false
}

func (e *Evaluator) faas(ctx context.Context, m *model.Monitor, now time.Time) (Result, error) {
	q := m.Query
	metric := strings.ToLower(q.Metric)
	valid := false
	for _, mm := range FaaSMetrics {
		valid = valid || mm == metric
	}
	if !valid {
		return Result{NoData: true}, fmt.Errorf("%w: faas metric %q", ErrUnsupported, q.Metric)
	}
	service := q.Filter.Service
	if service == "" && q.FunctionID == "" {
		service = m.Target.Service
	}
	// Postgres is needed to map a service to its functions, for cold starts (function → service) and for timeouts.
	var fns []model.Function
	needPG := service != "" || metric == "cold_starts" || q.RelativeToTimeout
	if needPG {
		if e.Functions == nil {
			return Result{NoData: true}, fmt.Errorf("%w: faas monitor needs serverless_functions", ErrUnsupported)
		}
		var err error
		if fns, err = e.Functions.ServerlessFunctions(ctx, int64(m.ProjectID), service, q.FunctionID); err != nil {
			return Result{}, err
		}
		if len(fns) == 0 {
			return Result{NoData: true}, nil
		}
	}
	byID := map[string]model.Function{}
	for _, f := range fns {
		if (q.Filter.Provider == "" || f.Provider == q.Filter.Provider) && (q.Filter.Region == "" || f.Region == q.Filter.Region) {
			byID[f.Slug] = f
		}
	}
	from, to := window(now, m.Condition.Window)
	c := Condition(m)
	var vals []EntityValue
	invs := map[string]float64{}

	if metric == "cold_starts" {
		services := map[string][]string{}
		for id, f := range byID {
			svc := f.Service
			if svc == "" {
				svc = f.Slug
			}
			services[svc] = append(services[svc], id)
		}
		if len(services) == 0 {
			return Result{NoData: true}, nil
		}
		list := make([]string, 0, len(services))
		for s := range services {
			list = append(list, s)
		}
		in, args := inList("service", list)
		w := tenantWhere(m).add("ts >= toDateTime64(?, 6) AND ts < toDateTime64(?, 6)", from.Unix(), to.Unix()).
			add("is_entry = 1").add("attrs['faas.coldstart'] = 'true'").add(in, args...)
		rows, err := e.CH.Query(ctx, "SELECT service, toFloat64(sum(sample_weight)) FROM telemetry.spans WHERE "+w.sql()+" GROUP BY service", w.args...)
		if err != nil {
			return Result{}, err
		}
		counts := map[string]float64{}
		for rows.Next() {
			var s string
			var n float64
			if err := rows.Scan(&s, &n); err != nil {
				rows.Close()
				return Result{}, err
			}
			counts[s] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return Result{}, err
		}
		for svc, ids := range services {
			for _, id := range ids {
				f := byID[id]
				vals = append(vals, EntityValue{Labels: map[string]string{"id": id, "service": svc, "provider": f.Provider, "region": f.Region}, Value: counts[svc]})
			}
		}
	} else {
		w := tenantWhere(m).between("minute", from, to).eq("provider", q.Filter.Provider).eq("region", q.Filter.Region)
		if needPG {
			ids := make([]string, 0, len(byID))
			for id := range byID {
				ids = append(ids, id)
			}
			if len(ids) == 0 {
				return Result{NoData: true}, nil
			}
			in, args := inList("function_id", ids)
			w.add(in, args...)
		} else {
			w.eq("function_id", q.FunctionID)
		}
		rows, err := e.CH.Query(ctx, `SELECT function_id, any(provider), any(region), max(invocations), max(errors), max(throttles),
			max(duration_p95), max(duration_p99), max(concurrency), max(iterator_age_ms)
			FROM telemetry.faas_1m WHERE `+w.sql()+` GROUP BY function_id, minute`, w.args...)
		if err != nil {
			return Result{}, err
		}
		type fn struct {
			provider, region string
			mins             []FaaSMinute
		}
		per := map[string]*fn{}
		for rows.Next() {
			var id, prov, reg string
			var mm FaaSMinute
			if err := rows.Scan(&id, &prov, &reg, &mm.Invocations, &mm.Errors, &mm.Throttles, &mm.P95, &mm.P99, &mm.Concurrency, &mm.IteratorAge); err != nil {
				rows.Close()
				return Result{}, err
			}
			if per[id] == nil {
				per[id] = &fn{provider: prov, region: reg}
			}
			per[id].mins = append(per[id].mins, mm)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return Result{}, err
		}
		for id, f := range per {
			a := AggregateFaaS(f.mins)
			v, ok := FaaSValue(metric, a, byID[id].TimeoutS, q.RelativeToTimeout)
			if !ok {
				continue
			}
			labels := map[string]string{"id": id, "provider": f.provider, "region": f.region}
			if s := byID[id].Service; s != "" {
				labels["service"] = s
			}
			vals = append(vals, EntityValue{Labels: labels, Value: v})
			invs[id] = a.Invocations
		}
	}
	if len(vals) == 0 {
		return Result{NoData: true}, nil
	}
	// minRequests: rate / duration values of functions with fewer invocations do not decide the breach
	candidates := vals
	rateLike := metric == "error_rate" || strings.HasPrefix(metric, "duration_")
	if rateLike && c.MinRequests > 0 {
		var busy []EntityValue
		for _, v := range vals {
			if invs[v.Labels["id"]] >= c.MinRequests {
				busy = append(busy, v)
			}
		}
		candidates = busy
	}
	lowTraffic := len(candidates) == 0
	if lowTraffic {
		candidates = vals
	}
	v, top := Worst(candidates, model.LowerIsWorse(c.Op), MaxDetailEntities)
	r := compare(m, v)
	if lowTraffic {
		r.Breached = false
	}
	ents := make([]map[string]any, len(top))
	for i, t := range top {
		d := t.detail()
		if n, ok := invs[t.Labels["id"]]; ok {
			d["invocations"] = n
		}
		ents[i] = d
	}
	r.Detail["metric"], r.Detail["entities"], r.Detail["matched"] = metric, ents, len(vals)
	if q.RelativeToTimeout {
		r.Detail["relativeToTimeout"] = true
	}
	return r, nil
}
