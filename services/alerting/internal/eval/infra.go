package eval

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
)

// Monitor kind `infra` (contracts §20): hosts_1m, containers_1m, k8s_pods_1m, k8s_nodes_1m, k8s_workloads_1m.
// Every entity matching query.filter gets a value over the window; the monitor value is the worst one (max, or min for
// '<' conditions) and detail.entities lists the worst 5. Gauges that were not reported are -Inf in the tables and are
// skipped (contracts §5). No matching entity with data → no_data.

// MaxDetailEntities is the number of entities kept in detail.entities.
const MaxDetailEntities = 5

// stateFresh: a pod/node/workload whose newest sample in the window is older than the window's newest sample by more
// than this has disappeared (deleted pod, removed node) and is not counted by not_ready / unavailable.
const stateFresh = 2 * time.Minute

// InfraMetrics lists the metrics each infra entity supports.
var InfraMetrics = map[string][]string{
	"host":      {"cpu", "memory", "disk", "load1"},
	"container": {"cpu_ratio", "memory_ratio", "restarts", "throttled"},
	"pod":       {"cpu_ratio", "memory_ratio", "restarts", "throttled", "not_ready"},
	"node":      {"not_ready"},
	"workload":  {"unavailable"},
}

// EntityValue is one entity's value (detail.entities element: labels + id + value).
type EntityValue struct {
	Labels map[string]string
	Value  float64
}

func (v EntityValue) detail() map[string]any {
	out := map[string]any{"value": v.Value}
	for k, s := range v.Labels {
		if s != "" {
			out[k] = s
		}
	}
	return out
}

// entityID builds the human id of an entity from its labels.
func entityID(entity string, l map[string]string) string {
	join := func(parts ...string) string {
		var p []string
		for _, s := range parts {
			if s != "" {
				p = append(p, s)
			}
		}
		return strings.Join(p, "/")
	}
	switch entity {
	case "host":
		return l["host"]
	case "container":
		if l["pod"] == "" {
			return join(l["host"], l["container"])
		}
		return join(l["namespace"], l["pod"], l["container"])
	case "pod":
		return join(l["namespace"], l["pod"])
	case "node":
		return l["node"]
	case "workload":
		return join(l["namespace"], l["kind"], l["name"])
	}
	return ""
}

// Worst picks the worst entity value (max, or min when lower is worse) and returns the top n entities, worst first.
func Worst(vals []EntityValue, lowerIsWorse bool, n int) (float64, []EntityValue) {
	if len(vals) == 0 {
		return math.NaN(), nil
	}
	s := append([]EntityValue(nil), vals...)
	sort.SliceStable(s, func(i, j int) bool {
		if lowerIsWorse {
			return s[i].Value < s[j].Value
		}
		return s[i].Value > s[j].Value
	})
	if len(s) > n {
		s = s[:n]
	}
	return s[0].Value, s
}

func (e *Evaluator) infra(ctx context.Context, m *model.Monitor, now time.Time) (Result, error) {
	entity := strings.ToLower(m.Query.Entity)
	metric := strings.ToLower(m.Query.Metric)
	ok := false
	for _, mm := range InfraMetrics[entity] {
		ok = ok || mm == metric
	}
	if !ok {
		return Result{NoData: true}, fmt.Errorf("%w: infra entity %q metric %q", ErrUnsupported, m.Query.Entity, m.Query.Metric)
	}
	agg := strings.ToLower(m.Query.Agg)
	if agg != "max" {
		agg = "avg"
	}
	from, to := window(now, m.Condition.Window)
	f := m.Query.Filter
	if f.Service == "" {
		f.Service = m.Target.Service
	}
	vals, count, err := e.infraValues(ctx, m, entity, metric, agg, f, from, to)
	if err != nil {
		return Result{}, err
	}
	if vals == nil {
		return Result{NoData: true}, nil
	}
	c := Condition(m)
	var v float64
	var top []EntityValue
	if count {
		// not_ready: the value is the number of entities that are not ready; detail lists them
		v = float64(len(vals))
		_, top = Worst(vals, false, MaxDetailEntities)
	} else {
		v, top = Worst(vals, model.LowerIsWorse(c.Op), MaxDetailEntities)
	}
	r := compare(m, v)
	ents := make([]map[string]any, len(top))
	for i, t := range top {
		ents[i] = t.detail()
	}
	r.Detail["entity"], r.Detail["metric"], r.Detail["entities"] = entity, metric, ents
	if !count {
		r.Detail["agg"], r.Detail["matched"] = agg, len(vals)
	}
	return r, nil
}

// infraWhere builds the tenant/window/filter predicate for table (columns differ per table; filters a table cannot
// answer directly go through containers_1m / k8s_pods_1m sub-selects).
func infraWhere(m *model.Monitor, table string, f model.InfraFilter, from, to time.Time) *where {
	w := tenantWhere(m).between("minute", from, to)
	sub := func(src, col string, conds map[string]string) {
		s := tenantWhere(m).between("minute", from, to)
		n := 0
		for _, k := range []string{"cluster", "namespace", "workload", "service"} {
			if v := conds[k]; v != "" {
				s.eq(k, v)
				n++
			}
		}
		if n > 0 {
			w.add(col+" IN (SELECT DISTINCT "+col+" FROM telemetry."+src+" WHERE "+s.sql()+")", s.args...)
		}
	}
	switch table {
	case "hosts_1m":
		w.eq("cluster", f.Cluster).eq("host", f.Host)
		sub("containers_1m", "host", map[string]string{"namespace": f.Namespace, "workload": f.Workload, "service": f.Service})
	case "containers_1m":
		w.eq("cluster", f.Cluster).eq("namespace", f.Namespace).eq("host", f.Host).eq("workload", f.Workload).eq("service", f.Service)
	case "k8s_pods_1m":
		w.eq("cluster", f.Cluster).eq("namespace", f.Namespace).eq("node", f.Host).eq("workload", f.Workload).eq("service", f.Service)
	case "k8s_nodes_1m":
		w.eq("cluster", f.Cluster).eq("node", f.Host)
		sub("k8s_pods_1m", "node", map[string]string{"namespace": f.Namespace, "workload": f.Workload, "service": f.Service})
	case "k8s_workloads_1m":
		w.eq("cluster", f.Cluster).eq("namespace", f.Namespace).eq("name", f.Workload)
		if f.Service != "" {
			s := tenantWhere(m).between("minute", from, to).eq("cluster", f.Cluster).eq("service", f.Service)
			w.add("(namespace, name) IN (SELECT DISTINCT namespace, workload FROM telemetry.k8s_pods_1m WHERE "+s.sql()+")", s.args...)
		}
	}
	return w
}

type infraShape struct {
	table      string
	full, keys []string // per-minute key (full) and entity key (keys ⊆ full)
	a, b       string   // value columns (b: ratio denominator)
}

func infraShapeOf(entity, metric string) infraShape {
	switch entity {
	case "host":
		return infraShape{table: "hosts_1m", full: []string{"cluster", "host"}, keys: []string{"cluster", "host"}, a: metric}
	case "node":
		return infraShape{table: "k8s_nodes_1m", full: []string{"cluster", "node"}, keys: []string{"cluster", "node"}, a: "ready"}
	case "workload":
		k := []string{"cluster", "namespace", "kind", "name"}
		return infraShape{table: "k8s_workloads_1m", full: k, keys: k, a: "desired", b: "available"}
	}
	ctr := []string{"cluster", "namespace", "pod", "container", "host"}
	s := infraShape{table: "containers_1m", full: ctr, keys: ctr}
	if entity == "pod" {
		s.keys = []string{"cluster", "namespace", "pod"}
		if metric == "restarts" || metric == "not_ready" {
			k := []string{"cluster", "namespace", "pod", "pod_uid"}
			s = infraShape{table: "k8s_pods_1m", full: k, keys: k}
		}
	}
	switch metric {
	case "cpu_ratio":
		s.a, s.b = "cpu", "cpu_limit"
	case "memory_ratio":
		s.a, s.b = "memory_bytes", "memory_limit"
	case "not_ready":
		s.a = "ready"
	default: // restarts, throttled
		s.a = metric
	}
	return s
}

// infraValues returns one value per matching entity (nil = no data). count reports a counting metric (not_ready),
// whose vals are only the offending entities (non-nil, possibly empty, when entities exist).
func (e *Evaluator) infraValues(ctx context.Context, m *model.Monitor, entity, metric, agg string, f model.InfraFilter, from, to time.Time) ([]EntityValue, bool, error) {
	s := infraShapeOf(entity, metric)
	w := infraWhere(m, s.table, f, from, to)
	full, keys := strings.Join(s.full, ", "), strings.Join(s.keys, ", ")
	var q string
	state := metric == "not_ready" || metric == "unavailable"
	switch {
	case state:
		inner := "max(" + s.a + ") AS a, -inf AS b, '' AS ph"
		if s.b != "" {
			inner = "max(" + s.a + ") AS a, max(" + s.b + ") AS b, '' AS ph"
		}
		if s.table == "k8s_pods_1m" {
			inner = "max(ready) AS a, -inf AS b, anyLast(phase) AS ph"
		}
		cond := "isFinite(a)"
		if s.b != "" {
			cond += " AND isFinite(b)"
		}
		q = fmt.Sprintf(`SELECT %[1]s, max(minute), argMax(a, minute), argMax(b, minute), argMax(ph, minute) FROM (
			SELECT %[1]s, minute, %[2]s FROM telemetry.%[3]s WHERE %[4]s GROUP BY %[1]s, minute) WHERE %[5]s GROUP BY %[1]s`,
			full, inner, s.table, w.sql(), cond)
	case metric == "restarts":
		q = fmt.Sprintf(`SELECT %[1]s, maxIf(r, isFinite(r)) - minIf(r, isFinite(r)) FROM (
			SELECT %[2]s, minute, max(%[3]s) AS r FROM telemetry.%[4]s WHERE %[5]s GROUP BY %[2]s, minute)
			GROUP BY %[1]s HAVING countIf(isFinite(r)) > 0`, keys, full, s.a, s.table, w.sql())
	case s.b != "": // ratio: Σ usage / Σ limit over the entity's containers that report both, per minute
		ok := "isFinite(a) AND isFinite(b) AND b > 0"
		q = fmt.Sprintf(`SELECT %[1]s, %[2]s(v) FROM (
			SELECT %[1]s, minute, if(sumIf(b, %[3]s) > 0, sumIf(a, %[3]s) / sumIf(b, %[3]s), nan) AS v FROM (
				SELECT %[4]s, minute, max(%[5]s) AS a, max(%[6]s) AS b FROM telemetry.%[7]s WHERE %[8]s GROUP BY %[4]s, minute)
			GROUP BY %[1]s, minute) WHERE isFinite(v) GROUP BY %[1]s`, keys, agg, ok, full, s.a, s.b, s.table, w.sql())
	default: // gauge (max over the entity's containers per minute)
		q = fmt.Sprintf(`SELECT %[1]s, %[2]s(v) FROM (
			SELECT %[1]s, minute, max(a) AS v FROM (
				SELECT %[3]s, minute, max(%[4]s) AS a FROM telemetry.%[5]s WHERE %[6]s GROUP BY %[3]s, minute)
			GROUP BY %[1]s, minute) WHERE isFinite(v) GROUP BY %[1]s`, keys, agg, full, s.a, s.table, w.sql())
	}
	rows, err := e.CH.Query(ctx, q, w.args...)
	if err != nil {
		return nil, state, err
	}
	defer rows.Close()
	cols := s.keys
	if state {
		cols = s.full
	}
	type stateRow struct {
		labels  map[string]string
		last    time.Time
		a, b    float64
		phase   string
	}
	var vals []EntityValue
	var states []stateRow
	for rows.Next() {
		strs := make([]string, len(cols))
		dest := make([]any, 0, len(cols)+4)
		for i := range strs {
			dest = append(dest, &strs[i])
		}
		var sr stateRow
		var v float64
		if state {
			dest = append(dest, &sr.last, &sr.a, &sr.b, &sr.phase)
		} else {
			dest = append(dest, &v)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, state, err
		}
		labels := make(map[string]string, len(cols)+1)
		for i, c := range cols {
			labels[c] = strs[i]
		}
		labels["id"] = entityID(entity, labels)
		if state {
			sr.labels = labels
			states = append(states, sr)
		} else if !math.IsNaN(v) && !math.IsInf(v, 0) {
			vals = append(vals, EntityValue{Labels: labels, Value: v})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, state, err
	}
	if !state {
		return vals, false, nil
	}
	if len(states) == 0 {
		return nil, true, nil
	}
	var newest time.Time
	for _, sr := range states {
		if sr.last.After(newest) {
			newest = sr.last
		}
	}
	out := []EntityValue{}
	for _, sr := range states {
		if newest.Sub(sr.last) > stateFresh {
			continue
		}
		if metric == "unavailable" {
			out = append(out, EntityValue{Labels: sr.labels, Value: math.Max(0, sr.a-sr.b)})
			continue
		}
		if sr.a < 1 && sr.phase != "Succeeded" { // completed Job pods are not "not ready"
			if sr.phase != "" {
				sr.labels["phase"] = sr.phase
			}
			out = append(out, EntityValue{Labels: sr.labels, Value: 1})
		}
	}
	return out, metric == "not_ready", nil
}
