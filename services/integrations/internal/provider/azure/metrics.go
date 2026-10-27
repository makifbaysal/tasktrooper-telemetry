package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tasktrooper/telemetry/services/integrations/internal/httpx"
	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
)

// Azure Monitor metric → trooper.faas.* (contracts §21). Every datapoint of the window is emitted (Azure metrics
// arrive 1–3 min late; faas_1m keeps the max per minute, so resends are idempotent).
type metricMap struct {
	azure, agg, name string
	scale            float64
}

var metricsByPlan = map[bool][]metricMap{
	// Consumption / Premium / Dedicated.
	false: {
		{"FunctionExecutionCount", "total", model.FaaSInvocations, 1},
		{"FunctionExecutionUnits", "total", model.FaaSExecutionUnits, 1},
	},
	// Flex Consumption has no FunctionExecutionCount/Units; its on-demand + always-ready series are summed.
	true: {
		{"OnDemandFunctionExecutionCount", "total", model.FaaSInvocations, 1},
		{"AlwaysReadyFunctionExecutionCount", "total", model.FaaSInvocations, 1},
		{"OnDemandFunctionExecutionUnits", "total", model.FaaSExecutionUnits, 1},
		{"AlwaysReadyFunctionExecutionUnits", "total", model.FaaSExecutionUnits, 1},
	},
}

var common = []metricMap{
	{"Http5xx", "total", model.FaaSErrors, 1},
	{"HttpResponseTime", "average", model.FaaSDurationAvg, 1000}, // seconds → ms
	{"HttpResponseTime", "maximum", model.FaaSDurationMax, 1000},
	{"MemoryWorkingSet", "average", model.FaaSMemoryBytes, 1},
}

// Metrics returns the Azure Monitor metrics read for a plan (exported for docs/tests).
func Metrics(plan string) []metricMap {
	return append(append([]metricMap(nil), metricsByPlan[plan == "Flex"]...), common...)
}

type metricsResponse struct {
	Value []struct {
		Name struct {
			Value string `json:"value"`
		} `json:"name"`
		Timeseries []struct {
			Data []map[string]any `json:"data"`
		} `json:"timeseries"`
	} `json:"value"`
}

// unsupported remembers metric names an app rejected (400), so they are not requested again.
type unsupported struct {
	mu sync.Mutex
	m  map[string]map[string]bool
}

func (u *unsupported) has(app, metric string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.m[app][metric]
}

func (u *unsupported) add(app, metric string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.m == nil {
		u.m = map[string]map[string]bool{}
	}
	if u.m[app] == nil {
		u.m[app] = map[string]bool{}
	}
	u.m[app][metric] = true
}

// Collect implements model.Provider.
func (p *Provider) Collect(ctx context.Context, inv *model.Inventory, now time.Time) (*model.Result, error) {
	st, _ := inv.State.(*state)
	res := &model.Result{}
	if st == nil {
		return res, nil
	}
	end := now.UTC().Truncate(time.Minute)
	start := end.Add(-10 * time.Minute)
	out := make([][]model.Sample, len(st.apps))
	errs := make([]error, len(st.apps))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, a := range st.apps {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			out[i], errs[i] = p.collectApp(ctx, a, start, end)
		}()
	}
	wg.Wait()
	var all error
	for i := range st.apps {
		res.Samples = append(res.Samples, out[i]...)
		if errs[i] != nil {
			all = errors.Join(all, fmt.Errorf("%s: metrics: %w", st.apps[i].Name, errs[i]))
		}
	}
	return res, all
}

func (p *Provider) collectApp(ctx context.Context, a app, start, end time.Time) ([]model.Sample, error) {
	maps := Metrics(a.Plan)
	var names []string
	aggs := map[string]bool{}
	for _, m := range maps {
		if p.unsupported.has(a.ID, m.azure) {
			continue
		}
		if len(names) == 0 || names[len(names)-1] != m.azure {
			names = append(names, m.azure)
		}
		aggs[m.agg] = true
	}
	if len(names) == 0 {
		return nil, nil
	}
	aggsOf := func(set map[string]bool) string {
		var l []string
		for k := range set {
			l = append(l, strings.ToUpper(k[:1])+k[1:])
		}
		sort.Strings(l)
		return strings.Join(l, ",")
	}
	get := func(metrics []string, aggregation string) (*metricsResponse, error) {
		q := url.Values{
			"api-version": {apiMetrics}, "metricnames": {strings.Join(metrics, ",")}, "interval": {"PT1M"},
			"timespan": {start.Format(time.RFC3339) + "/" + end.Format(time.RFC3339)}, "aggregation": {aggregation},
		}
		var r metricsResponse
		err := p.arm.Get(ctx, a.ID+"/providers/microsoft.insights/metrics", q, &r)
		return &r, err
	}
	resp, err := get(names, aggsOf(aggs))
	if httpx.IsStatus(err, http.StatusBadRequest) && len(names) > 1 {
		// One unsupported metric (or aggregation) fails the whole request: retry one by one with its own aggregations and remember the rejected ones.
		resp, err = &metricsResponse{}, nil
		for _, n := range names {
			own := map[string]bool{}
			for _, m := range maps {
				if m.azure == n {
					own[m.agg] = true
				}
			}
			r, e := get([]string{n}, aggsOf(own))
			if httpx.IsStatus(e, http.StatusBadRequest) {
				p.unsupported.add(a.ID, n)
				continue
			}
			if e != nil {
				err = errors.Join(err, e)
				continue
			}
			resp.Value = append(resp.Value, r.Value...)
		}
	}
	if resp == nil {
		return nil, err
	}
	return Map(a, maps, resp), err
}

// Map turns an Azure Monitor response into samples; several Azure metrics mapped to one trooper metric (Flex
// on-demand + always-ready) are summed per minute.
func Map(a app, maps []metricMap, r *metricsResponse) []model.Sample {
	type key struct {
		name string
		ts   time.Time
	}
	sums := map[key]float64{}
	var order []key
	for _, v := range r.Value {
		for _, m := range maps {
			if !strings.EqualFold(m.azure, v.Name.Value) {
				continue
			}
			for _, series := range v.Timeseries {
				for _, d := range series.Data {
					val, ok := d[m.agg].(float64)
					if !ok {
						continue
					}
					tsStr, _ := d["timeStamp"].(string)
					ts, err := time.Parse(time.RFC3339, tsStr)
					if err != nil {
						continue
					}
					k := key{m.name, ts.UTC().Truncate(time.Minute)}
					if _, seen := sums[k]; !seen {
						order = append(order, k)
					}
					sums[k] += val * m.scale
				}
			}
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].name != order[j].name {
			return order[i].name < order[j].name
		}
		return order[i].ts.Before(order[j].ts)
	})
	out := make([]model.Sample, 0, len(order))
	attrs := model.FaaSAttrs(a.Slug, a.Name)
	for _, k := range order {
		out = append(out, model.Sample{Name: k.name, Value: sums[k], TS: k.ts, Region: a.Region, Attrs: attrs})
	}
	return out
}
