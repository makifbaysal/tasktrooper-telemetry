package store

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

// mapEdgeRow is one aggregated outgoing call (edges_1m) or incoming call (callers_1m) for the service map.
type mapEdgeRow struct {
	from, to, toKind, toSystem string
	toTier                     string
	calls, errors              float64
	p95                        float64
}

// ServiceMap returns the whole-system graph: client apps → services → dependencies, with traffic per edge and
// health (plus the worst open incident) per node. Incoming edges come from callers_1m (the callee's view), outgoing
// dependencies from edges_1m; an http dependency that resolves to an instrumented service is left to callers_1m.
func (d *DB) ServiceMap(ctx context.Context, q Q) (*model.ServiceMap, error) {
	services, err := d.ListServices(ctx, q)
	if err != nil {
		return nil, err
	}
	mins := q.W.Minutes()
	ec, ea := envCond(q.Env, "env")
	window := []any{secs(q.W.From), secs(q.W.To)}

	var rows []mapEdgeRow
	in, err := d.ch.Query(ctx, `SELECT service, caller, any(caller_kind), toFloat64(sum(count)), toFloat64(sum(errors)),
	    arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(0.5, 0.95)(duration))
	  FROM callers_1m WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)`+ec+`
	  GROUP BY service, caller`, append(window, ea...)...)
	if err != nil {
		return nil, err
	}
	for in.Next() {
		var svc, caller, kind string
		var c, e float64
		var qs []float64
		if err := in.Scan(&svc, &caller, &kind, &c, &e, &qs); err != nil {
			in.Close()
			return nil, err
		}
		if caller == "" || kind == "" || kind == "unknown" {
			continue
		}
		// the caller is the edge's source: reuse the row with from/to swapped (the "to" fields describe the source node)
		rows = append(rows, mapEdgeRow{from: caller, to: svc, toKind: kind, toTier: callerTier(kind), calls: c, errors: e, p95: qt(qs, 1)})
	}
	in.Close()
	if err := in.Err(); err != nil {
		return nil, err
	}

	out, err := d.ch.Query(ctx, `SELECT parent_service, dep, dep_kind, child_service, toFloat64(sum(calls)), toFloat64(sum(errors)),
	    arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(0.5, 0.95)(duration))
	  FROM edges_1m WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)`+ec+`
	  GROUP BY parent_service, dep, dep_kind, child_service`, append(window, ea...)...)
	if err != nil {
		return nil, err
	}
	var deps []mapEdgeRow
	for out.Next() {
		var parent, dep, kind, child string
		var c, e float64
		var qs []float64
		if err := out.Scan(&parent, &dep, &kind, &child, &c, &e, &qs); err != nil {
			out.Close()
			return nil, err
		}
		deps = append(deps, depEdge(parent, dep, kind, child, c, e, qt(qs, 1)))
	}
	out.Close()
	if err := out.Err(); err != nil {
		return nil, err
	}

	incidents, err := d.mapIncidents(ctx)
	if err != nil {
		return nil, err
	}
	return buildServiceMap(services, rows, deps, incidents, mins), nil
}

func callerTier(kind string) string {
	if kind == "service" {
		return "service"
	}
	return "client"
}

// depEdge maps an edges_1m row: http calls into an instrumented service become service → service edges, everything
// else points at a dependency node ("system · name" → system + name).
func depEdge(parent, dep, kind, child string, calls, errs, p95 float64) mapEdgeRow {
	if kind == "http" && child != "" {
		return mapEdgeRow{from: parent, to: child, toKind: "service", toTier: "service", calls: calls, errors: errs, p95: p95}
	}
	system, name := "", dep
	if i := strings.Index(dep, " · "); i > 0 {
		system, name = dep[:i], dep[i+len(" · "):]
	}
	tier := "data"
	if kind == "http" {
		tier = "external"
	}
	return mapEdgeRow{from: parent, to: "dep:" + dep, toKind: kind, toSystem: system + "\x00" + name, toTier: tier, calls: calls, errors: errs, p95: p95}
}

// mapIncidents returns the worst open incident per service (critical firing first, then newest) and the open count.
func (d *DB) mapIncidents(ctx context.Context) (map[string]*model.MapIncident, error) {
	rows, err := d.pg.Query(ctx, `SELECT id, coalesce(service, ''), title, severity, state FROM incidents
	  WHERE {{tenant}} AND state <> 'resolved' AND coalesce(service, '') <> '' ORDER BY started_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*model.MapIncident{}
	for rows.Next() {
		var id int64
		var svc, title, sev, st string
		if err := rows.Scan(&id, &svc, &title, &sev, &st); err != nil {
			return nil, err
		}
		cur := out[svc]
		inc := &model.MapIncident{ID: strconv.FormatInt(id, 10), Title: title, Severity: sev, State: st}
		if cur == nil {
			inc.Open = 1
			out[svc] = inc
			continue
		}
		cur.Open++
		if incidentRank(inc) > incidentRank(cur) {
			inc.Open = cur.Open
			out[svc] = inc
		}
	}
	return out, rows.Err()
}

func incidentRank(i *model.MapIncident) int {
	r := 0
	if i.Severity == "critical" {
		r += 2
	}
	if i.State == "firing" {
		r++
	}
	return r
}

// edgeHealth grades an edge (or a dependency node) by its error rate alone; there is no baseline per edge.
func edgeHealth(errRate float64) model.Health {
	switch {
	case errRate >= 0.05:
		return model.Critical
	case errRate >= 0.01:
		return model.Warning
	}
	return model.OK
}

// buildServiceMap assembles nodes and edges; pure so it is table-tested. Duplicate edges (same from → to) are merged.
func buildServiceMap(services []model.Service, callers, deps []mapEdgeRow, incidents map[string]*model.MapIncident, mins float64) *model.ServiceMap {
	nodes := map[string]*model.MapNode{}
	for _, s := range services {
		tier := "service"
		if s.Type == "web" || s.Type == "mobile" {
			tier = "client"
		}
		nodes[s.ID] = &model.MapNode{ID: s.ID, Name: s.Name, Tier: tier, Kind: s.Type, Status: s.Status,
			Rpm: s.Rpm, ErrorRate: s.ErrorRate, P95: s.P95, Incident: incidents[s.ID]}
	}
	type acc struct {
		from, to            string
		calls, errors, p95w float64
	}
	edges := map[string]*acc{}
	depTotals := map[string]*acc{}
	add := func(r mapEdgeRow, sourceIsNew bool) {
		// the node this row introduces: the caller (incoming rows) or the dependency (outgoing rows)
		id := r.to
		if sourceIsNew {
			id = r.from
		}
		if _, ok := nodes[id]; !ok {
			n := &model.MapNode{ID: id, Name: id, Tier: r.toTier, Kind: r.toKind, Status: model.OK}
			if r.toSystem != "" {
				sys, name, _ := strings.Cut(r.toSystem, "\x00")
				n.System, n.Name = sys, name
			}
			nodes[id] = n
		}
		other := r.from
		if sourceIsNew {
			other = r.to
		}
		if _, ok := nodes[other]; !ok { // a service seen only through traffic
			nodes[other] = &model.MapNode{ID: other, Name: other, Tier: "service", Kind: "service", Status: model.OK}
		}
		key := r.from + "→" + r.to
		e := edges[key]
		if e == nil {
			e = &acc{from: r.from, to: r.to}
			edges[key] = e
		}
		e.calls += r.calls
		e.errors += r.errors
		e.p95w += r.p95 * r.calls
		if !sourceIsNew && strings.HasPrefix(r.to, "dep:") {
			t := depTotals[r.to]
			if t == nil {
				t = &acc{}
				depTotals[r.to] = t
			}
			t.calls += r.calls
			t.errors += r.errors
			t.p95w += r.p95 * r.calls
		}
	}
	seen := map[string]bool{}
	for _, r := range callers {
		seen[r.from+"→"+r.to] = true
		add(r, true)
	}
	for _, r := range deps {
		if r.toTier == "service" && seen[r.from+"→"+r.to] {
			continue // the callee's view (callers_1m) already has this edge
		}
		if _, known := nodes[r.to]; r.toTier == "service" && !known {
			// peer.service named a callee that sends no telemetry (a third-party API): an external node
			r.to, r.toKind, r.toTier, r.toSystem = "dep:http · "+r.to, "http", "external", "http\x00"+r.to
		}
		add(r, false)
	}
	for id, t := range depTotals {
		n := nodes[id]
		n.Rpm, n.ErrorRate, n.P95 = t.calls/mins, div(t.errors, t.calls), div(t.p95w, t.calls)
		n.Status = edgeHealth(n.ErrorRate)
	}

	m := &model.ServiceMap{Nodes: make([]model.MapNode, 0, len(nodes)), Edges: make([]model.MapEdge, 0, len(edges))}
	for _, n := range nodes {
		m.Nodes = append(m.Nodes, *n)
	}
	for k, e := range edges {
		er := div(e.errors, e.calls)
		m.Edges = append(m.Edges, model.MapEdge{ID: k, From: e.from, To: e.to, CallsPerMin: e.calls / mins,
			ErrorRate: er, P95: div(e.p95w, e.calls), Status: edgeHealth(er)})
	}
	sort.Slice(m.Nodes, func(i, j int) bool { return m.Nodes[i].ID < m.Nodes[j].ID })
	sort.Slice(m.Edges, func(i, j int) bool { return m.Edges[i].ID < m.Edges[j].ID })
	return m
}
