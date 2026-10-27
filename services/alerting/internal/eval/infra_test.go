package eval

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
)

func TestWorst(t *testing.T) {
	vals := []EntityValue{{Value: 0.2}, {Value: 0.9}, {Value: 0.5}}
	cases := []struct {
		lower bool
		n     int
		want  float64
		top   []float64
	}{
		{false, 5, 0.9, []float64{0.9, 0.5, 0.2}},
		{false, 2, 0.9, []float64{0.9, 0.5}},
		{true, 1, 0.2, []float64{0.2}},
	}
	for _, tc := range cases {
		v, top := Worst(vals, tc.lower, tc.n)
		var got []float64
		for _, e := range top {
			got = append(got, e.Value)
		}
		if v != tc.want || !slices.Equal(got, tc.top) {
			t.Errorf("lower=%v n=%d: %v %v", tc.lower, tc.n, v, got)
		}
	}
	if v, top := Worst(nil, false, 5); !math.IsNaN(v) || top != nil {
		t.Error("empty")
	}
}

func TestEntityID(t *testing.T) {
	cases := []struct {
		entity string
		labels map[string]string
		want   string
	}{
		{"host", map[string]string{"host": "h1", "cluster": "c"}, "h1"},
		{"container", map[string]string{"namespace": "shop", "pod": "api-1", "container": "api", "host": "h1"}, "shop/api-1/api"},
		{"container", map[string]string{"container": "redis", "host": "h1"}, "h1/redis"},
		{"pod", map[string]string{"namespace": "shop", "pod": "api-1"}, "shop/api-1"},
		{"node", map[string]string{"node": "n1"}, "n1"},
		{"workload", map[string]string{"namespace": "shop", "kind": "Deployment", "name": "api"}, "shop/Deployment/api"},
	}
	for _, tc := range cases {
		if got := entityID(tc.entity, tc.labels); got != tc.want {
			t.Errorf("%s %v: %q want %q", tc.entity, tc.labels, got, tc.want)
		}
	}
}

func TestInfraShapeAndWhere(t *testing.T) {
	m := &model.Monitor{OrgID: 1, ProjectID: 2}
	from, to := time.Unix(600, 0), time.Unix(900, 0)
	cases := []struct {
		entity, metric, table string
		f                     model.InfraFilter
		wantSQL               []string
		nargs                 int
	}{
		{"host", "cpu", "hosts_1m", model.InfraFilter{Cluster: "c"}, []string{"cluster = ?"}, 5},
		{"host", "cpu", "hosts_1m", model.InfraFilter{Service: "orders"}, []string{"host IN (SELECT DISTINCT host FROM telemetry.containers_1m", "service = ?"}, 9},
		{"container", "cpu_ratio", "containers_1m", model.InfraFilter{Namespace: "shop", Workload: "api"}, []string{"namespace = ?", "workload = ?"}, 6},
		{"pod", "restarts", "k8s_pods_1m", model.InfraFilter{Host: "n1"}, []string{"node = ?"}, 5},
		{"pod", "cpu_ratio", "containers_1m", model.InfraFilter{}, nil, 4},
		{"node", "not_ready", "k8s_nodes_1m", model.InfraFilter{Service: "orders"}, []string{"node IN (SELECT DISTINCT node FROM telemetry.k8s_pods_1m"}, 9},
		{"workload", "unavailable", "k8s_workloads_1m", model.InfraFilter{Workload: "api", Service: "s"}, []string{"name = ?", "(namespace, name) IN"}, 10},
	}
	for _, tc := range cases {
		s := infraShapeOf(tc.entity, tc.metric)
		if s.table != tc.table {
			t.Errorf("%s/%s: table %s want %s", tc.entity, tc.metric, s.table, tc.table)
		}
		w := infraWhere(m, s.table, tc.f, from, to)
		sql := w.sql()
		for _, frag := range tc.wantSQL {
			if !strings.Contains(sql, frag) {
				t.Errorf("%s/%s: %q lacks %q", tc.entity, tc.metric, sql, frag)
			}
		}
		if strings.Count(sql, "?") != len(w.args) || len(w.args) != tc.nargs {
			t.Errorf("%s/%s: %d placeholders, %d args (want %d): %s", tc.entity, tc.metric, strings.Count(sql, "?"), len(w.args), tc.nargs, sql)
		}
	}
}

func TestInfraUnsupported(t *testing.T) {
	e := &Evaluator{}
	for _, q := range []model.Query{{Entity: "host", Metric: "restarts"}, {Entity: "galaxy", Metric: "cpu"}, {Entity: "node", Metric: "cpu"}} {
		r, err := e.Eval(t.Context(), &model.Monitor{Kind: "infra", Query: q}, time.Now())
		if err == nil || !r.NoData {
			t.Errorf("%+v: %v %+v", q, err, r)
		}
	}
	r, err := e.Eval(t.Context(), &model.Monitor{Kind: "faas", Query: model.Query{Metric: "nope"}}, time.Now())
	if err == nil || !r.NoData {
		t.Errorf("faas bad metric: %v", err)
	}
	// service-scoped faas without a Postgres lookup
	r, err = e.Eval(t.Context(), &model.Monitor{Kind: "faas", Target: model.Target{Service: "s"}, Query: model.Query{Metric: "errors"}}, time.Now())
	if err == nil || !r.NoData {
		t.Errorf("faas without lookup: %v", err)
	}
}

func TestAggregateFaaS(t *testing.T) {
	inf := math.Inf(-1)
	mins := []FaaSMinute{
		{Invocations: 100, Errors: 5, Throttles: inf, P95: 200, P99: 400, Concurrency: 3, IteratorAge: inf},
		{Invocations: 300, Errors: 0, Throttles: inf, P95: 100, P99: inf, Concurrency: 7, IteratorAge: inf},
		{Invocations: inf, Errors: inf, Throttles: inf, P95: inf, P99: inf, Concurrency: inf, IteratorAge: inf},
	}
	a := AggregateFaaS(mins)
	cases := []struct {
		metric   string
		timeout  float64
		relative bool
		want     float64
		ok       bool
	}{
		{"invocations", 0, false, 400, true},
		{"errors", 0, false, 5, true},
		{"error_rate", 0, false, 5.0 / 400, true},
		{"throttles", 0, false, 0, false}, // never reported (Azure)
		{"duration_p95", 0, false, (100*200 + 300*100) / 400.0, true},
		{"duration_p99", 0, false, 400, true},
		{"duration_p95", 1, true, 0.125, true},
		{"duration_p95", 0, true, 0, false}, // no timeout known
		{"concurrency", 0, false, 7, true},
		{"iterator_age_ms", 0, false, 0, false},
	}
	for _, tc := range cases {
		v, ok := FaaSValue(tc.metric, a, tc.timeout, tc.relative)
		if ok != tc.ok || (ok && math.Abs(v-tc.want) > 1e-9) {
			t.Errorf("%s (timeout %v relative %v): %v %v, want %v %v", tc.metric, tc.timeout, tc.relative, v, ok, tc.want, tc.ok)
		}
	}
	// no invocations at all: p95 is the plain mean
	b := AggregateFaaS([]FaaSMinute{{Invocations: inf, P95: 10}, {Invocations: inf, P95: 30}})
	if b.P95 != 20 {
		t.Errorf("unweighted p95 = %v", b.P95)
	}
	if _, ok := FaaSValue("error_rate", b, 0, false); ok {
		t.Error("error_rate without invocations")
	}
}

// Every template decodes into a monitor the evaluator accepts.
func TestTemplates(t *testing.T) {
	seen := map[string]bool{}
	for _, tp := range Templates {
		if seen[tp.ID] {
			t.Errorf("duplicate template %s", tp.ID)
		}
		seen[tp.ID] = true
		qb, _ := json.Marshal(tp.Query)
		cb, _ := json.Marshal(tp.Condition)
		var q model.Query
		var c model.Condition
		if err := json.Unmarshal(qb, &q); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(cb, &c); err != nil {
			t.Fatal(err)
		}
		switch tp.Kind {
		case "infra":
			if !slices.Contains(InfraMetrics[q.Entity], q.Metric) {
				t.Errorf("%s: %s/%s unsupported", tp.ID, q.Entity, q.Metric)
			}
		case "faas":
			if !slices.Contains(FaaSMetrics, q.Metric) {
				t.Errorf("%s: metric %s", tp.ID, q.Metric)
			}
		default:
			t.Errorf("%s: kind %s", tp.ID, tp.Kind)
		}
		if c.Window <= 0 || c.Op == "" {
			t.Errorf("%s: condition %+v", tp.ID, c)
		}
	}
	if !seen["faas-near-timeout"] || !seen["k8s-pod-crashloop"] {
		t.Error("missing templates")
	}
}
