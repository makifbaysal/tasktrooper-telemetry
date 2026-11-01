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

// Monitor kind `infra` (contracts §20): hosts_1m, containers_1m, k8s_pods_1m, k8s_nodes_1m, k8s_workloads_1m and
// db_1m (entity database). Node cpu/memory/disk come from the agent's host rows on that node (hosts_1m.node); a cluster
// value is the average over its nodes per minute.
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
	"node":      {"not_ready", "cpu", "memory", "disk"},
	"cluster":   {"cpu", "memory", "disk"},
	"workload":  {"unavailable"},
	"database":  {"cpu", "memory", "disk", "connections_ratio", "p95_ms", "repl_lag_ms", "hit_ratio", "evictions", "slow"},
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
	case "cluster":
		return l["cluster"]
	case "database":
		return join(l["database_id"], l["node"])
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
	case "hosts_1m:node": // Kubernetes nodes: host rows the agent tagged with k8s.node.name; filter.host names the node
		w.eq("cluster", f.Cluster).eq("node", f.Host).add("node != ''")
		sub("k8s_pods_1m", "node", map[string]string{"namespace": f.Namespace, "workload": f.Workload, "service": f.Service})
	case "hosts_1m:cluster":
		w.eq("cluster", f.Cluster).add("cluster != ''")
	case "db_1m":
		w.eq("database_id", f.Database).eq("engine", f.Engine)
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
	table      string // ClickHouse table; where (if set) picks the infraWhere variant of a shared table
	where      string
	minuteAgg  string   // how entity rows combine per minute: max (default) or avg (cluster = mean over its nodes)
	full, keys []string // per-minute key (full) and entity key (keys ⊆ full)
	a, b       string   // value columns (b: ratio denominator)
}

func infraShapeOf(entity, metric string) infraShape {
	switch entity {
	case "host":
		return infraShape{table: "hosts_1m", full: []string{"cluster", "host"}, keys: []string{"cluster", "host"}, a: metric}
	case "node":
		if metric != "not_ready" {
			return infraShape{table: "hosts_1m", where: "hosts_1m:node", full: []string{"cluster", "node", "host"}, keys: []string{"cluster", "node"}, a: metric}
		}
		return infraShape{table: "k8s_nodes_1m", full: []string{"cluster", "node"}, keys: []string{"cluster", "node"}, a: "ready"}
	case "cluster":
		return infraShape{table: "hosts_1m", where: "hosts_1m:cluster", full: []string{"cluster", "host"}, keys: []string{"cluster"}, a: metric, minuteAgg: "avg"}
	case "database":
		s := infraShape{table: "db_1m", full: []string{"database_id", "node"}, keys: []string{"database_id", "node"}, a: metric}
		if metric == "connections_ratio" {
			s.a, s.b = "connections", "max_connections"
		}
		return s
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
	variant := s.table
	if s.where != "" {
		variant = s.where
	}
	w := infraWhere(m, variant, f, from, to)
	// -Inf marks an unreported gauge: max() skips it on its own (and stays -Inf → filtered when nothing reported);
	// the mean must skip it explicitly (avgIf over no rows is nan → filtered).
	minuteAgg := "max(a)"
	if s.minuteAgg == "avg" {
		minuteAgg = "avgIf(a, isFinite(a))"
	}
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
	default: // gauge: per minute the max over the entity's rows (containers, a node's hosts) or their mean (cluster)
		q = fmt.Sprintf(`SELECT %[1]s, %[2]s(v) FROM (
			SELECT %[1]s, minute, %[7]s AS v FROM (
				SELECT %[3]s, minute, max(%[4]s) AS a FROM telemetry.%[5]s WHERE %[6]s GROUP BY %[3]s, minute)
			GROUP BY %[1]s, minute) WHERE isFinite(v) GROUP BY %[1]s`, keys, agg, full, s.a, s.table, w.sql(), minuteAgg)
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
		labels map[string]string
		last   time.Time
		a, b   float64
		phase  string
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
			if sr.labels["kind"] == "Job" { // a running Job reports succeeded pods as available
				continue
			}
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
