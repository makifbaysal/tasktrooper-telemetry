package store

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Databases and broker clusters that only exist in trooper-agent metrics (a static agent.yaml target, no Postgres
// row) are listed too: the collector writes `trooper.db.id` → db_1m.database_id and `trooper.broker.cluster` →
// broker_*_1m.cluster_id (contracts §5). Registered rows win; an observed-only target gets its engine / kind from
// the metrics and a name that lines up with the services' dependency names where that is unambiguous.

// observedWindow is the lookback for observed-only targets: the query window, at least the last 10 minutes.
func observedFrom(q Q) time.Time {
	from := q.W.From
	if recent := q.Now.Add(-10 * time.Minute); from.IsZero() || recent.Before(from) {
		from = recent
	}
	return from
}

// EngineOf maps the collector's db.system (postgresql|mysql|redis|mongodb|clickhouse) to the registry engine.
func EngineOf(system string) string {
	switch s := strings.ToLower(system); s {
	case "postgresql", "postgres", "pg":
		return "postgres"
	default:
		return s
	}
}

type observedDB struct{ id, system string }

func (d *DB) observedDatabases(ctx context.Context, from time.Time) ([]observedDB, error) {
	rows, err := d.ch.Query(ctx, `SELECT database_id, anyIf(engine, engine != '') FROM db_1m
	  WHERE {{tenant}} AND minute >= toDateTime(?) AND database_id != '' GROUP BY database_id ORDER BY database_id`, from.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []observedDB
	for rows.Next() {
		var o observedDB
		if err := rows.Scan(&o.id, &o.system); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// depTargets returns the distinct dependency names of a kind set ("postgresql · trooper") since from.
func (d *DB) depTargets(ctx context.Context, from time.Time, kinds []string) ([]string, error) {
	rows, err := d.ch.Query(ctx, `SELECT DISTINCT dep FROM edges_1m WHERE {{tenant}} AND dep_kind IN (?) AND minute >= toDateTime(?) ORDER BY dep`,
		kinds, from.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// allDbMetas returns registered databases plus observed-only ones (optionally just slug).
func (d *DB) allDbMetas(ctx context.Context, q Q, slug string) ([]dbMeta, error) {
	metas, err := d.dbMetas(ctx, "")
	if err != nil {
		return nil, err
	}
	from := observedFrom(q)
	obs, err := d.observedDatabases(ctx, from)
	if err != nil {
		return nil, err
	}
	var deps []string
	if len(obs) > 0 {
		if deps, err = d.depTargets(ctx, q.Now.Add(-24*time.Hour), []string{"db", "cache"}); err != nil {
			return nil, err
		}
	}
	all := MergeObservedDBs(metas, obs, deps)
	if slug == "" {
		return all, nil
	}
	for _, m := range all {
		if m.slug == slug {
			return []dbMeta{m}, nil
		}
	}
	return nil, nil
}

// MergeObservedDBs appends observed-only databases to the registered ones (sorted by name). The name of an
// observed database is the dependency target services use for it ("postgresql · trooper" → trooper) when the id
// itself is such a target or when it is the only unregistered database of its engine and exactly one target of that
// engine is unclaimed; otherwise the id.
func MergeObservedDBs(metas []dbMeta, obs []observedDB, deps []string) []dbMeta {
	registered := map[string]bool{}
	claimed := map[string]map[string]bool{} // engine → registered names
	for _, m := range metas {
		registered[m.slug] = true
		if claimed[m.engine] == nil {
			claimed[m.engine] = map[string]bool{}
		}
		claimed[m.engine][m.name] = true
	}
	var extra []dbMeta
	for _, o := range obs {
		if registered[o.id] {
			continue
		}
		registered[o.id] = true
		extra = append(extra, dbMeta{slug: o.id, name: o.id, engine: EngineOf(o.system), collector: "agent", observed: true, meta: map[string]any{}})
	}
	if len(extra) == 0 {
		return metas
	}
	cands := map[string][]string{} // engine → unclaimed dependency targets
	for _, dep := range deps {
		sys, target, ok := strings.Cut(dep, " · ")
		if !ok || target == "" {
			continue
		}
		e := EngineOf(sys)
		if claimed[e][target] {
			continue
		}
		cands[e] = appendUnique(cands[e], target)
	}
	named := map[int]bool{}
	for i := range extra {
		e := extra[i].engine
		for j, c := range cands[e] {
			if c == extra[i].slug {
				named[i] = true
				cands[e] = append(cands[e][:j:j], cands[e][j+1:]...)
				break
			}
		}
	}
	left := map[string][]int{}
	for i := range extra {
		if !named[i] {
			left[extra[i].engine] = append(left[extra[i].engine], i)
		}
	}
	for e, idx := range left {
		if len(idx) == 1 && len(cands[e]) == 1 {
			extra[idx[0]].name = cands[e][0]
		}
	}
	out := append(append([]dbMeta{}, metas...), extra...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}

// ---------------------------------------------------------------- brokers

type observedCluster struct {
	id            string
	nodes, topics []string
}

func (d *DB) observedClusters(ctx context.Context, from time.Time) ([]observedCluster, error) {
	rows, err := d.ch.Query(ctx, `SELECT c, groupUniqArrayIf(n, n != ''), groupUniqArrayIf(t, t != '') FROM (
	    SELECT cluster_id AS c, node AS n, '' AS t FROM broker_nodes_1m WHERE {{tenant}} AND minute >= toDateTime(?)
	    UNION ALL SELECT cluster_id AS c, '' AS n, topic AS t FROM broker_lag_1m WHERE {{tenant}} AND minute >= toDateTime(?)
	    UNION ALL SELECT cluster_id AS c, '' AS n, topic AS t FROM broker_topics_1m WHERE {{tenant}} AND minute >= toDateTime(?))
	  WHERE c != '' GROUP BY c ORDER BY c`, from.Unix(), from.Unix(), from.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []observedCluster
	for rows.Next() {
		var o observedCluster
		if err := rows.Scan(&o.id, &o.nodes, &o.topics); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// allClusterMetas returns registered clusters plus observed-only ones (optionally just slug).
func (d *DB) allClusterMetas(ctx context.Context, now time.Time, slug string) ([]clusterMeta, error) {
	metas, err := d.clusterMetas(ctx, "")
	if err != nil {
		return nil, err
	}
	obs, err := d.observedClusters(ctx, now.Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	var deps []string
	if len(obs) > 0 {
		if deps, err = d.depTargets(ctx, now.Add(-24*time.Hour), []string{"queue"}); err != nil {
			return nil, err
		}
	}
	all := MergeObservedClusters(metas, obs, deps)
	if slug == "" {
		return all, nil
	}
	for _, m := range all {
		if m.slug == slug {
			return []clusterMeta{m}, nil
		}
	}
	return nil, nil
}

// MergeObservedClusters appends observed-only clusters (name = id) to the registered ones. Kind: the messaging
// system services use for one of the cluster's topics ("kafka · tt.spans"), else rabbitmq for RabbitMQ node names
// (rabbit@host), else kafka.
func MergeObservedClusters(metas []clusterMeta, obs []observedCluster, deps []string) []clusterMeta {
	registered := map[string]bool{}
	for _, m := range metas {
		registered[m.slug] = true
	}
	topicSys := map[string]string{}
	for _, dep := range deps {
		if sys, t, ok := strings.Cut(dep, " · "); ok && BrokerKind(sys) != "" {
			topicSys[t] = BrokerKind(sys)
		}
	}
	out := append([]clusterMeta{}, metas...)
	added := false
	for _, o := range obs {
		if registered[o.id] {
			continue
		}
		registered[o.id] = true
		added = true
		out = append(out, clusterMeta{slug: o.id, name: o.id, kind: inferKind(o, topicSys), collector: "agent", observed: true})
	}
	if added {
		sort.SliceStable(out, func(i, j int) bool { return out[i].name < out[j].name })
	}
	return out
}

func inferKind(o observedCluster, topicSys map[string]string) string {
	ts := append([]string{}, o.topics...)
	sort.Strings(ts)
	for _, t := range ts {
		if k := topicSys[t]; k != "" {
			return k
		}
	}
	for _, n := range o.nodes {
		if strings.HasPrefix(n, "rabbit@") {
			return "rabbitmq"
		}
	}
	return "kafka"
}
