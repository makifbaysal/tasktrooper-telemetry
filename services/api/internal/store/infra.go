package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

var collectorLabel = map[string]string{"agent": "TaskTrooper agent", "cloud": "Cloud integration", "direct": "Direct connection"}

// DefaultPort per engine.
var DefaultPort = map[string]int{"postgres": 5432, "mysql": 3306, "redis": 6379, "mongodb": 27017}

// depNames are the dependency names services use for a database ("postgres · orders-db").
func depNames(engine, name string) []string {
	systems := map[string][]string{"postgres": {"postgres", "postgresql"}, "mysql": {"mysql"}, "redis": {"redis"}, "mongodb": {"mongodb"}}[engine]
	out := make([]string, 0, len(systems))
	for _, s := range systems {
		out = append(out, s+" · "+name)
	}
	return out
}

func isUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// ---------------------------------------------------------------- databases

type dbMeta struct {
	id                       int64
	slug, name, engine, host string
	port                     int
	collector, creds, cloud  string
	env, user                string
	created                  time.Time
	// cloud discovery metadata (deploy/postgres/004)
	integration, source, version, zone, class string
	meta                                      map[string]any
	diskGb                                    float64
	missing                                   bool
}

func (d *DB) dbMetas(ctx context.Context, slug string) ([]dbMeta, error) {
	where, args := "", []any{}
	if slug != "" {
		where, args = " AND slug = $1", []any{slug}
	}
	rows, err := d.pg.Query(ctx, `SELECT id, slug, name, engine, host, port, collector, coalesce(credentials_ref, ''), coalesce(cloud_resource_id, ''), env, coalesce(username, ''), created_at,
	    coalesce(integration_id::text, ''), coalesce(source, ''), coalesce(version, ''), coalesce(zone, ''), coalesce(instance_class, ''), coalesce(disk_gb, 0),
	    coalesce((meta->>'missing')::boolean, false), coalesce(meta, '{}'::jsonb)
	  FROM databases WHERE {{tenant}}`+where+` ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dbMeta
	for rows.Next() {
		var m dbMeta
		if err := rows.Scan(&m.id, &m.slug, &m.name, &m.engine, &m.host, &m.port, &m.collector, &m.creds, &m.cloud, &m.env, &m.user, &m.created,
			&m.integration, &m.source, &m.version, &m.zone, &m.class, &m.diskGb, &m.missing, &m.meta); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

type nodeStat struct {
	db, node, role                                                 string
	cpu, memory, disk, conns, maxConns, ops, p95, slow, hit, evict float64
	repl                                                           float64
}

// latestNodes reads the most recent minute per database node (last 10 minutes).
func (d *DB) latestNodes(ctx context.Context, now time.Time, slug string) (map[string][]nodeStat, error) {
	where, args := "", []any{now.Add(-10 * time.Minute).Unix()}
	if slug != "" {
		where, args = " AND database_id = ?", append(args, slug)
	}
	cols := []string{"cpu", "memory", "disk", "connections", "max_connections", "ops", "p95_ms", "slow", "hit_ratio", "evictions", "repl_lag_ms"}
	var inner, outer []string
	for _, c := range cols {
		agg := "maxIf"
		if c == "hit_ratio" {
			agg = "minIf"
		}
		inner = append(inner, fmt.Sprintf("%s(%s, isFinite(%s)) AS %s", agg, c, c, c))
		outer = append(outer, fmt.Sprintf("toFloat64(argMax(%s, m))", c))
	}
	rows, err := d.ch.Query(ctx, `SELECT database_id, node, any(role), `+strings.Join(outer, ", ")+` FROM (
	    SELECT database_id, node, any(role) AS role, minute AS m, `+strings.Join(inner, ", ")+`
	    FROM db_1m WHERE {{tenant}} AND minute >= toDateTime(?)`+where+` GROUP BY database_id, node, m)
	  GROUP BY database_id, node ORDER BY database_id, node`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]nodeStat{}
	for rows.Next() {
		var n nodeStat
		if err := rows.Scan(&n.db, &n.node, &n.role, &n.cpu, &n.memory, &n.disk, &n.conns, &n.maxConns, &n.ops, &n.p95, &n.slow, &n.hit, &n.evict, &n.repl); err != nil {
			return nil, err
		}
		out[n.db] = append(out[n.db], n)
	}
	return out, rows.Err()
}

func (d *DB) prevDbStats(ctx context.Context, w timerange.Window) (map[string][2]float64, error) {
	rows, err := d.ch.Query(ctx, `SELECT database_id, toFloat64(avgIf(cpu, isFinite(cpu))), toFloat64(avgIf(p95_ms, isFinite(p95_ms)))
	  FROM db_1m WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?) GROUP BY database_id`, secs(w.From), secs(w.To))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]float64{}
	for rows.Next() {
		var id string
		var c, p float64
		if err := rows.Scan(&id, &c, &p); err != nil {
			return nil, err
		}
		out[id] = [2]float64{fin(c), fin(p)}
	}
	return out, rows.Err()
}

func buildDatabase(m dbMeta, nodes []nodeStat, prev [2]float64) model.Database {
	db := model.Database{
		ID: m.slug, Name: m.name, Engine: m.engine, Host: m.host, Port: m.port, Env: m.env, Collector: m.collector,
		Source: collectorLabel[m.collector], AddedAt: ms(m.created), Since: ms(m.created), CredentialsRef: m.creds, CloudResourceID: m.cloud, User: m.user,
		Nodes: []model.DbNode{}, Status: model.OK, PrevCPU: prev[0], PrevP95: prev[1],
		Version: m.version, Zone: m.zone, InstanceClass: m.class, DiskGb: m.diskGb, IntegrationID: m.integration, Missing: m.missing, Meta: dbMetaPublic(m.meta),
	}
	if m.source != "" {
		db.Source = m.source
	}
	if len(nodes) == 0 {
		db.NoData = true
		return db
	}
	var primary *nodeStat
	evict := 0.0
	db.HitRatio = 1
	for i := range nodes {
		n := &nodes[i]
		n.cpu, n.memory, n.disk, n.conns, n.maxConns = fin(n.cpu), fin(n.memory), fin(n.disk), fin(n.conns), fin(n.maxConns)
		role := n.role
		if role == "" {
			role = "primary"
		}
		if role == "primary" && primary == nil {
			primary = n
		}
		dn := model.DbNode{ID: n.node, Role: role, Zone: m.zone, CPU: n.cpu, Memory: n.memory, Connections: n.conns}
		if role == "replica" {
			v := fin(n.repl)
			dn.ReplLagMs = &v
		}
		db.Nodes = append(db.Nodes, dn)
		db.CPU = math.Max(db.CPU, n.cpu)
		db.Memory = math.Max(db.Memory, n.memory)
		db.Disk = math.Max(db.Disk, n.disk)
		db.Connections += n.conns
		db.MaxConnections = math.Max(db.MaxConnections, n.maxConns)
		db.OpsPerSec += fin(n.ops)
		db.P95 = math.Max(db.P95, fin(n.p95))
		db.SlowPerMin += fin(n.slow)
		if h := fin(n.hit); h > 0 {
			db.HitRatio = math.Min(db.HitRatio, h)
		}
		evict += fin(n.evict)
	}
	if primary != nil { // the primary carries the load that matters for CPU / memory
		db.CPU, db.Memory = primary.cpu, primary.memory
	}
	if m.engine == "redis" {
		db.EvictionsPerMin = &evict
	}
	db.Status, db.Problem = dbHealth(db)
	return db
}

// dbHealth applies capacity rules to a database snapshot.
func dbHealth(db model.Database) (model.Health, *model.Problem) {
	pct := func(v float64) string { return strconv.Itoa(int(math.Round(v*100))) + "%" }
	conn := div(db.Connections, db.MaxConnections)
	kv := db.Engine == "redis"
	type rule struct {
		hit   bool
		h     model.Health
		title string
		fix   string
	}
	evict := 0.0
	if db.EvictionsPerMin != nil {
		evict = *db.EvictionsPerMin
	}
	for _, r := range []rule{
		{conn >= 0.95, model.Critical, "Connections at " + pct(conn) + " of max_connections", "Put a connection pooler (PgBouncer / ProxySQL) in front, or lower client pool sizes."},
		{db.CPU >= 0.9, model.Critical, "CPU at " + pct(db.CPU), "Check the top queries below for the biggest share of database time; add the missing index or scale up."},
		{db.Disk >= 0.9, model.Critical, "Disk at " + pct(db.Disk), "Grow the volume or reduce retention before writes fail."},
		{kv && db.Memory >= 0.95, model.Critical, "Memory at " + pct(db.Memory) + " of maxmemory", "Set TTLs on large key families or move to a bigger node."},
		{conn >= 0.8, model.Warning, "Connections at " + pct(conn) + " of max_connections", "Put a connection pooler in front, or lower client pool sizes before the next scale-out."},
		{db.CPU >= 0.75, model.Warning, "CPU at " + pct(db.CPU), "Look at the top queries by share of database time."},
		{db.Disk >= 0.8, model.Warning, "Disk at " + pct(db.Disk), "Plan a volume increase or shorter retention."},
		{kv && (db.Memory >= 0.85 || evict > 0), model.Warning, "Memory at " + pct(db.Memory) + " of maxmemory" + map[bool]string{true: " — evicting keys", false: ""}[evict > 0], "Set TTLs on keys without one, or move to a bigger node."},
		{!kv && db.HitRatio > 0 && db.HitRatio < 0.9, model.Warning, "Buffer cache hit ratio " + pct(db.HitRatio), "The working set no longer fits in memory: add RAM or reduce scans."},
		{db.PrevP95 > 0 && db.P95 > 3*db.PrevP95 && db.P95 > 20, model.Warning, fmt.Sprintf("Query p95 %.0f ms (was %.0f ms)", db.P95, db.PrevP95), "Compare the top queries' plans with the previous period; a dropped index or a new query is likely."},
	} {
		if r.hit {
			detail := fmt.Sprintf("%.0f of %.0f connections in use, CPU %s, memory %s.", db.Connections, db.MaxConnections, pct(db.CPU), pct(db.Memory))
			return r.h, &model.Problem{Title: r.title, Detail: detail, Fix: r.fix}
		}
	}
	return model.OK, nil
}

// ListDatabases returns registered databases with their current metrics.
func (d *DB) ListDatabases(ctx context.Context, q Q) ([]model.Database, error) {
	metas, err := d.dbMetas(ctx, "")
	if err != nil {
		return nil, err
	}
	nodes, err := d.latestNodes(ctx, q.Now, "")
	if err != nil {
		return nil, err
	}
	prev, err := d.prevDbStats(ctx, q.W.Prev())
	if err != nil {
		return nil, err
	}
	out := make([]model.Database, 0, len(metas))
	for _, m := range metas {
		out = append(out, buildDatabase(m, nodes[m.slug], prev[m.slug]))
	}
	return out, nil
}

// GetDatabase returns one database by slug.
func (d *DB) GetDatabase(ctx context.Context, q Q, slug string) (*model.Database, error) {
	metas, err := d.dbMetas(ctx, slug)
	if err != nil {
		return nil, err
	}
	if len(metas) == 0 {
		return nil, ErrNotFound
	}
	nodes, err := d.latestNodes(ctx, q.Now, slug)
	if err != nil {
		return nil, err
	}
	prev, err := d.prevDbStats(ctx, q.W.Prev())
	if err != nil {
		return nil, err
	}
	db := buildDatabase(metas[0], nodes[slug], prev[slug])
	return &db, nil
}

var dbMetricCols = map[string]string{
	"cpu": "cpu", "memory": "memory", "disk": "disk", "connections": "connections", "ops": "ops", "p95": "p95_ms",
	"hit": "hit_ratio", "repl": "repl_lag_ms", "slow": "slow", "evictions": "evictions",
}

// DatabaseSeries returns one metric per node (or one node) over time.
func (d *DB) DatabaseSeries(ctx context.Context, q Q, slug, metric, node string) (model.SeriesSet, error) {
	col, ok := dbMetricCols[metric]
	if !ok {
		return model.SeriesSet{}, Invalid("metric must be cpu|memory|disk|connections|ops|p95|hit|repl|slow|evictions")
	}
	if _, err := d.GetDatabase(ctx, q, slug); err != nil {
		return model.SeriesSet{}, err
	}
	agg := "maxIf"
	if col == "hit_ratio" {
		agg = "minIf"
	}
	where, args := " AND database_id = ?", []any{secs(q.W.From), secs(q.W.To), slug}
	if node != "" {
		where += " AND node = ?"
		args = append(args, node)
	}
	rows, err := d.ch.Query(ctx, `SELECT node, `+q.W.BucketExpr("minute")+` AS b, toFloat64(`+agg+`(`+col+`, isFinite(`+col+`)))
	  FROM db_1m WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)`+where+` GROUP BY node, b ORDER BY node, b`, args...)
	if err != nil {
		return model.SeriesSet{}, err
	}
	defer rows.Close()
	b := timerange.NewBuilder(q.W)
	for rows.Next() {
		var n string
		var bk int64
		var v float64
		if err := rows.Scan(&n, &bk, &v); err != nil {
			return model.SeriesSet{}, err
		}
		b.Set(n, int(bk), v)
	}
	return b.Build(), rows.Err()
}

// DatabaseQueries returns the top queries / commands with the service + endpoint that sends them.
func (d *DB) DatabaseQueries(ctx context.Context, q Q, slug string) ([]model.DbQueryRow, error) {
	if _, err := d.GetDatabase(ctx, q, slug); err != nil {
		return nil, err
	}
	rows, err := d.ch.Query(ctx, `SELECT query_hash, any(query), `+queryKey("any(query)")+`, toFloat64(sum(calls)) AS c, toFloat64(sum(total_ms)) AS t, toFloat64(max(p95_ms)), toFloat64(sum(rows))
	  FROM db_queries_1m WHERE {{tenant}} AND database_id = ? AND minute >= toDateTime(?) AND minute < toDateTime(?)
	  GROUP BY query_hash ORDER BY t DESC LIMIT 50`, slug, secs(q.W.From), secs(q.W.To))
	if err != nil {
		return nil, err
	}
	out := []model.DbQueryRow{}
	var keys []uint64
	keyOf := map[uint64][]int{}
	total := 0.0
	for rows.Next() {
		var h, key uint64
		var r model.DbQueryRow
		var c, t, rws float64
		if err := rows.Scan(&h, &r.Text, &key, &c, &t, &r.P95, &rws); err != nil {
			rows.Close()
			return nil, err
		}
		r.ID = strconv.FormatUint(h, 10)
		r.CallsPerMin, r.AvgMs, r.Rows, r.Share = c/q.W.Minutes(), div(t, c), div(rws, c), t
		r.P95 = fin(r.P95)
		total += t
		keys = append(keys, key)
		keyOf[key] = append(keyOf[key], len(out))
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Share = div(out[i].Share, total)
	}
	if len(keys) == 0 {
		return out, nil
	}
	// Which endpoint sends each query. db_queries_1m.query_hash is the engine's query id, so queries are matched
	// to client spans by normalized text (db.query.text), not by spans.db_query_hash.
	srows, err := d.ch.Query(ctx, `SELECT k, service, entry_method, entry_route, c FROM (
	    SELECT `+queryKey("attrs['db.query.text']")+` AS k, service, entry_method, entry_route, toFloat64(sum(sample_weight)) AS c
	    FROM spans WHERE {{tenant}} AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?)
	      AND kind = 'client' AND dep_kind = 'db' AND attrs['db.query.text'] != ''
	    GROUP BY k, service, entry_method, entry_route)
	  WHERE k IN (?) ORDER BY c DESC LIMIT 1 BY k`,
		micros(q.W.From), micros(q.W.To), keys)
	if err != nil {
		return nil, err
	}
	type src struct {
		svc, method, route string
		c                  float64
	}
	by := map[int]src{}
	for srows.Next() {
		var k uint64
		var s src
		if err := srows.Scan(&k, &s.svc, &s.method, &s.route, &s.c); err != nil {
			srows.Close()
			return nil, err
		}
		for _, i := range keyOf[k] {
			by[i] = s
		}
	}
	srows.Close()
	for i := range out {
		s, ok := by[i]
		if !ok {
			continue
		}
		out[i].ServiceID = s.svc
		ref := &EndpointRef{Method: s.method, Route: s.route, Process: s.method == ""}
		out[i].Endpoint = ref.Key()
		n, _, err := d.entryCount(ctx, q.W, q.Env, s.svc, ref)
		if err != nil {
			return nil, err
		}
		out[i].PerCall = div(s.c, n)
	}
	return out, nil
}

// DatabaseUsers returns the instrumented services that call a database, from edges_1m.
func (d *DB) DatabaseUsers(ctx context.Context, q Q, slug string) ([]model.DbUser, error) {
	metas, err := d.dbMetas(ctx, slug)
	if err != nil {
		return nil, err
	}
	if len(metas) == 0 {
		return nil, ErrNotFound
	}
	names := depNames(metas[0].engine, metas[0].name)
	ec, ea := envCond(q.Env, "env")
	rows, err := d.ch.Query(ctx, `SELECT parent_service, dep, dep_kind, toFloat64(sum(calls)) AS c, toFloat64(sum(errors)), toFloat64(sum(duration_sum)),
	    arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(0.5, 0.95)(duration))
	  FROM edges_1m WHERE {{tenant}} AND dep IN (?) AND minute >= toDateTime(?) AND minute < toDateTime(?)`+ec+`
	  GROUP BY parent_service, dep, dep_kind ORDER BY c DESC`, append([]any{names, secs(q.W.From), secs(q.W.To)}, ea...)...)
	if err != nil {
		return nil, err
	}
	var edges []edgeRow
	var svcs []string
	for rows.Next() {
		var e edgeRow
		if err := rows.Scan(&e.route, &e.dep, &e.kind, &e.calls, &e.errors, &e.dsum, &e.q); err != nil {
			rows.Close()
			return nil, err
		}
		edges = append(edges, e)
		svcs = append(svcs, e.route) // parent service is carried in route here
	}
	rows.Close()
	out := []model.DbUser{}
	if len(edges) == 0 {
		return out, rows.Err()
	}
	all, err := d.ListServices(ctx, q)
	if err != nil {
		return nil, err
	}
	by := map[string]model.Service{}
	for _, s := range all {
		s.Spark = nil
		by[s.ID] = s
	}
	for _, e := range edges {
		svc, ok := by[e.route]
		if !ok {
			svc = model.Service{ID: e.route, Name: e.route, Type: "http", Status: model.OK}
		}
		out = append(out, model.DbUser{Service: svc, Dep: depFrom(e.route, e, q.W.Minutes())})
	}
	_ = svcs
	return out, nil
}

// CreateDatabase registers a database for the collector.
func (d *DB) CreateDatabase(ctx context.Context, q Q, in model.DatabaseInput) (*model.Database, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, Invalid("name is required")
	}
	if _, ok := DefaultPort[in.Engine]; !ok {
		return nil, Invalid("engine must be postgres|mysql|redis|mongodb")
	}
	if in.Collector == "" {
		in.Collector = "agent"
	}
	if collectorLabel[in.Collector] == "" {
		return nil, Invalid("collector must be agent|cloud|direct")
	}
	if strings.TrimSpace(in.Host) == "" {
		return nil, Invalid("host is required")
	}
	if in.Port == 0 {
		in.Port = DefaultPort[in.Engine]
	}
	if in.Port < 0 || in.Port > 65535 {
		return nil, Invalid("port out of range")
	}
	if in.Env == "" {
		in.Env = "production"
	}
	slug := LowerSlug(in.ID)
	if slug == "" {
		slug = LowerSlug(in.Name)
	}
	_, err := d.pg.Exec(ctx, `INSERT INTO databases (project_id, slug, name, engine, host, port, collector, credentials_ref, cloud_resource_id, env, username)
	  VALUES ({{project}}, $1, $2, $3, $4, $5, $6, nullif($7, ''), nullif($8, ''), $9, nullif($10, ''))`,
		slug, strings.TrimSpace(in.Name), in.Engine, in.Host, in.Port, in.Collector, in.CredentialsRef, in.CloudResourceID, in.Env, strings.TrimSpace(in.User))
	if isUnique(err) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}
	return d.GetDatabase(ctx, q, slug)
}

// DeleteDatabase unregisters a database.
func (d *DB) DeleteDatabase(ctx context.Context, slug string) error {
	tag, err := d.pg.Exec(ctx, `DELETE FROM databases WHERE {{tenant}} AND slug = $1`, slug)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DatabaseConn returns engine/host/port of a registered database (for connection tests).
func (d *DB) DatabaseConn(ctx context.Context, slug string) (model.ConnTestInput, error) {
	metas, err := d.dbMetas(ctx, slug)
	if err != nil {
		return model.ConnTestInput{}, err
	}
	if len(metas) == 0 {
		return model.ConnTestInput{}, ErrNotFound
	}
	m := metas[0]
	return model.ConnTestInput{Kind: m.engine, Host: m.host, Port: m.port, User: m.user}, nil
}

// ---------------------------------------------------------------- brokers

type clusterMeta struct {
	id                                       int64
	slug, name, kind, endpoint, vhost, creds string
	collector                                string
	auth                                     []byte
	created                                  time.Time
	integration, source, version             string
	missing                                  bool
}

func (d *DB) clusterMetas(ctx context.Context, slug string) ([]clusterMeta, error) {
	where, args := "", []any{}
	if slug != "" {
		where, args = " AND slug = $1", []any{slug}
	}
	rows, err := d.pg.Query(ctx, `SELECT id, slug, name, kind, endpoint, coalesce(vhost, ''), coalesce(credentials_ref, ''), collector, auth, created_at,
	    coalesce(integration_id::text, ''), coalesce(source, ''), coalesce(version, ''), coalesce((meta->>'missing')::boolean, false)
	  FROM broker_clusters WHERE {{tenant}}`+where+` ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []clusterMeta
	for rows.Next() {
		var m clusterMeta
		if err := rows.Scan(&m.id, &m.slug, &m.name, &m.kind, &m.endpoint, &m.vhost, &m.creds, &m.collector, &m.auth, &m.created,
			&m.integration, &m.source, &m.version, &m.missing); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (d *DB) brokerNodes(ctx context.Context, now time.Time) (map[string][]model.BrokerNode, error) {
	rows, err := d.ch.Query(ctx, `SELECT cluster_id, node, toFloat64(argMax(cpu, minute)), toFloat64(argMax(memory, minute)), toFloat64(argMax(disk, minute)), toFloat64(argMax(net_in, minute))
	  FROM (SELECT cluster_id, node, minute, maxIf(cpu, isFinite(cpu)) AS cpu, maxIf(memory, isFinite(memory)) AS memory, maxIf(disk, isFinite(disk)) AS disk, maxIf(net_in, isFinite(net_in)) AS net_in FROM broker_nodes_1m
	    WHERE {{tenant}} AND minute >= toDateTime(?) GROUP BY cluster_id, node, minute)
	  GROUP BY cluster_id, node ORDER BY cluster_id, node`, now.Add(-10*time.Minute).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]model.BrokerNode{}
	for rows.Next() {
		var c string
		var n model.BrokerNode
		if err := rows.Scan(&c, &n.ID, &n.CPU, &n.Memory, &n.Disk, &n.NetInMBs); err != nil {
			return nil, err
		}
		n.Host = n.ID
		n.CPU, n.Memory, n.Disk, n.NetInMBs = fin(n.CPU), fin(n.Memory), fin(n.Disk), fin(n.NetInMBs)
		switch {
		case n.CPU >= 0.9 || n.Disk >= 0.9 || n.Memory >= 0.95:
			n.Status = model.Critical
		case n.CPU >= 0.75 || n.Disk >= 0.8 || n.Memory >= 0.85:
			n.Status = model.Warning
		default:
			n.Status = model.OK
		}
		if n.Disk >= 0.8 {
			n.Note = fmt.Sprintf("Disk %d%% — rebalance partition leaders or shorten retention.", int(math.Round(n.Disk*100)))
		}
		out[c] = append(out[c], n)
	}
	return out, rows.Err()
}

// GroupFilter narrows consumer group listing.
type GroupFilter struct {
	ClusterID string
	Group     string
	ServiceID string
}

type lagCell struct {
	cluster, group, topic string
	partition             uint32
	lag, consume, produce float64
	unacked               float64
	oldestAge             float64 // seconds; -1 = not reported
}

// latestLag reads the most recent minute per (cluster, group, topic, partition) in the last 10 minutes.
func (d *DB) latestLag(ctx context.Context, now time.Time, f GroupFilter) ([]lagCell, error) {
	where, args := "", []any{now.Add(-10 * time.Minute).Unix()}
	if f.ClusterID != "" {
		where += " AND cluster_id = ?"
		args = append(args, f.ClusterID)
	}
	if f.Group != "" {
		where += " AND consumer_group = ?"
		args = append(args, f.Group)
	}
	rows, err := d.ch.Query(ctx, `SELECT cluster_id, consumer_group, topic, partition, toFloat64(argMax(l, m)), toFloat64(argMax(c, m)), toFloat64(argMax(p, m)), toFloat64(argMax(u, m)),
	    toFloat64(argMax(a, m))
	  FROM (SELECT cluster_id, consumer_group, topic, partition, minute AS m, maxIf(lag, isFinite(lag)) AS l, maxIf(consume_rate, isFinite(consume_rate)) AS c, maxIf(produce_rate, isFinite(produce_rate)) AS p, max(unacked) AS u,
	    if(countIf(isFinite(oldest_age_s)) > 0, maxIf(oldest_age_s, isFinite(oldest_age_s)), -1) AS a
	    FROM broker_lag_1m WHERE {{tenant}} AND minute >= toDateTime(?)`+where+` GROUP BY cluster_id, consumer_group, topic, partition, m)
	  GROUP BY cluster_id, consumer_group, topic, partition ORDER BY cluster_id, consumer_group, topic, partition`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lagCell
	for rows.Next() {
		var c lagCell
		if err := rows.Scan(&c.cluster, &c.group, &c.topic, &c.partition, &c.lag, &c.consume, &c.produce, &c.unacked, &c.oldestAge); err != nil {
			return nil, err
		}
		c.lag, c.consume, c.produce, c.unacked = fin(c.lag), fin(c.consume), fin(c.produce), fin(c.unacked)
		if math.IsNaN(c.oldestAge) || math.IsInf(c.oldestAge, 0) {
			c.oldestAge = -1
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type groupState struct {
	state               string
	members, rebalances float64
}

func (d *DB) groupStates(ctx context.Context, now time.Time) (map[string]groupState, error) {
	rows, err := d.ch.Query(ctx, `SELECT cluster_id, consumer_group, argMax(state, minute), toFloat64(argMax(members, minute)), toFloat64(argMax(rebalances, minute))
	  FROM broker_groups_1m WHERE {{tenant}} AND minute >= toDateTime(?) GROUP BY cluster_id, consumer_group`, now.Add(-10*time.Minute).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]groupState{}
	for rows.Next() {
		var c, g string
		var s groupState
		if err := rows.Scan(&c, &g, &s.state, &s.members, &s.rebalances); err != nil {
			return nil, err
		}
		// collector writes ±Inf for values it did not report (contracts §5)
		s.members, s.rebalances = fin(s.members), fin(s.rebalances)
		out[c+"/"+g] = s
	}
	return out, rows.Err()
}

// lagTrend classifies the last hour of a group's total lag.
func (d *DB) lagTrends(ctx context.Context, now time.Time) (map[string]string, error) {
	rows, err := d.ch.Query(ctx, `SELECT cluster_id, consumer_group, groupArray(tl) FROM (
	    SELECT cluster_id, consumer_group, m, sum(l) AS tl FROM (
	      SELECT cluster_id, consumer_group, topic, partition, minute AS m, maxIf(lag, isFinite(lag)) AS l FROM broker_lag_1m
	      WHERE {{tenant}} AND minute >= toDateTime(?) GROUP BY cluster_id, consumer_group, topic, partition, m)
	    GROUP BY cluster_id, consumer_group, m ORDER BY m)
	  GROUP BY cluster_id, consumer_group`, now.Add(-time.Hour).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var c, g string
		var xs []float64
		if err := rows.Scan(&c, &g, &xs); err != nil {
			return nil, err
		}
		out[c+"/"+g] = Trend(xs)
	}
	return out, rows.Err()
}

// Trend classifies a lag series: batch (climbs and drains to ~0 repeatedly), growing, shrinking or steady.
func Trend(xs []float64) string {
	if len(xs) < 4 {
		return "steady"
	}
	peak := 0.0
	for _, v := range xs {
		peak = math.Max(peak, v)
	}
	if peak == 0 {
		return "steady"
	}
	drains := 0
	for i := 1; i < len(xs); i++ {
		if xs[i] < peak*0.05 && xs[i-1] >= peak*0.5 {
			drains++
		}
	}
	if drains >= 1 && peak >= 1000 {
		return "batch"
	}
	third := len(xs) / 3
	avg := func(s []float64) float64 {
		t := 0.0
		for _, v := range s {
			t += v
		}
		return t / float64(len(s))
	}
	first, last := avg(xs[:third]), avg(xs[len(xs)-third:])
	switch {
	case last > first*1.2 && last-first > 100:
		return "growing"
	case last < first*0.8 && first-last > 100:
		return "shrinking"
	}
	return "steady"
}

type owner struct{ service, process, source string }

func (d *DB) groupOwners(ctx context.Context) (map[string]owner, error) {
	out := map[string]owner{}
	rows, err := d.pg.Query(ctx, `SELECT c.slug, o.consumer_group, coalesce(s.name, ''), coalesce(o.process, ''), o.source FROM consumer_group_owners o
	  JOIN broker_clusters c ON c.id = o.cluster_id LEFT JOIN services s ON s.id = o.service_id WHERE {{tenant:c}}`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c, g string
		var o owner
		if err := rows.Scan(&c, &g, &o.service, &o.process, &o.source); err != nil {
			return nil, err
		}
		out[c+"/"+g] = o
	}
	return out, rows.Err()
}

// observedOwners maps consumer group → (service, process) from SDK spans (consumer_1m).
func (d *DB) observedOwners(ctx context.Context, now time.Time) (map[string]owner, error) {
	rows, err := d.ch.Query(ctx, `SELECT consumer_group, any(service), any(process) FROM consumer_1m
	  WHERE {{tenant}} AND minute >= toDateTime(?) AND consumer_group != '' GROUP BY consumer_group`, now.Add(-24*time.Hour).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]owner{}
	for rows.Next() {
		var g string
		var o owner
		if err := rows.Scan(&g, &o.service, &o.process); err != nil {
			return nil, err
		}
		o.source = "observed"
		out[g] = o
	}
	return out, rows.Err()
}

func readHealth(trend, state string, lag float64) model.Health {
	switch {
	case trend == "batch":
		return model.OK
	case trend == "growing" && lag > 10000:
		return model.Critical
	case trend == "growing" || state == "Rebalancing":
		return model.Warning
	}
	return model.OK
}

func worst(hs ...model.Health) model.Health {
	rank := map[model.Health]int{model.OK: 0, model.Warning: 1, model.Critical: 2}
	w := model.OK
	for _, h := range hs {
		if rank[h] > rank[w] {
			w = h
		}
	}
	return w
}

// ListGroups returns consumer groups with per-topic, per-partition lag.
func (d *DB) ListGroups(ctx context.Context, now time.Time, f GroupFilter) ([]model.ConsumerGroup, error) {
	cells, err := d.latestLag(ctx, now, f)
	if err != nil {
		return nil, err
	}
	states, err := d.groupStates(ctx, now)
	if err != nil {
		return nil, err
	}
	trends, err := d.lagTrends(ctx, now)
	if err != nil {
		return nil, err
	}
	owners, err := d.groupOwners(ctx)
	if err != nil {
		return nil, err
	}
	observed, err := d.observedOwners(ctx, now)
	if err != nil {
		return nil, err
	}
	kinds := map[string]string{}
	metas, err := d.clusterMetas(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, m := range metas {
		kinds[m.slug] = m.kind
	}
	type key struct{ c, g string }
	order := []key{}
	by := map[key]*model.ConsumerGroup{}
	reads := map[key]map[string]*model.GroupRead{}
	for _, c := range cells {
		k := key{c.cluster, c.group}
		g, ok := by[k]
		if !ok {
			g = &model.ConsumerGroup{ClusterID: c.cluster, ID: c.group, Reads: []model.GroupRead{}}
			by[k] = g
			reads[k] = map[string]*model.GroupRead{}
			order = append(order, k)
		}
		r, ok := reads[k][c.topic]
		if !ok {
			g.Reads = append(g.Reads, model.GroupRead{Topic: c.topic, PartitionLag: []float64{}})
			r = &g.Reads[len(g.Reads)-1]
			reads[k][c.topic] = r
		}
		for int(c.partition) >= len(r.PartitionLag) {
			r.PartitionLag = append(r.PartitionLag, 0)
		}
		r.PartitionLag[c.partition] = c.lag
		r.ConsumePerSec += c.consume
		if c.oldestAge >= 0 {
			a := c.oldestAge
			if r.OldestAgeS != nil {
				a = math.Max(a, *r.OldestAgeS)
			}
			r.OldestAgeS = &a
		}
		if kinds[c.cluster] == "rabbitmq" {
			u := c.unacked
			if r.Unacked != nil {
				u += *r.Unacked
			}
			r.Unacked = &u
		}
		g.Lag += c.lag
		// reads slice may have grown; re-point map entries
		for i := range g.Reads {
			reads[k][g.Reads[i].Topic] = &g.Reads[i]
		}
	}
	out := []model.ConsumerGroup{}
	for _, k := range order {
		g := by[k]
		id := k.c + "/" + k.g
		st := states[id]
		g.State, g.Members, g.RebalancesPerHour = st.state, st.members, st.rebalances
		if g.State == "" {
			g.State = "Stable"
		}
		g.Trend = trends[id]
		if g.Trend == "" {
			g.Trend = "steady"
		}
		if o, ok := owners[id]; ok && o.service != "" {
			g.ServiceID, g.Process = o.service, o.process
		} else if o, ok := observed[k.g]; ok {
			g.ServiceID, g.Process = o.service, o.process
		} else {
			g.External = "Not instrumented (lag from the broker only)"
		}
		if f.ServiceID != "" && g.ServiceID != f.ServiceID {
			continue
		}
		hs := []model.Health{}
		for _, r := range g.Reads {
			lag := 0.0
			for _, v := range r.PartitionLag {
				lag += v
			}
			hs = append(hs, readHealth(g.Trend, g.State, lag))
		}
		g.Health = worst(hs...)
		out = append(out, *g)
	}
	return out, nil
}

// ListTopics returns topics/queues seen by the broker collector with their producers and total lag.
func (d *DB) ListTopics(ctx context.Context, now time.Time, cluster string) ([]model.Topic, error) {
	cells, err := d.latestLag(ctx, now, GroupFilter{ClusterID: cluster})
	if err != nil {
		return nil, err
	}
	groups, err := d.ListGroups(ctx, now, GroupFilter{ClusterID: cluster})
	if err != nil {
		return nil, err
	}
	health := map[string]model.Health{}
	for _, g := range groups {
		for _, r := range g.Reads {
			k := g.ClusterID + "/" + r.Topic
			lag := 0.0
			for _, v := range r.PartitionLag {
				lag += v
			}
			health[k] = worst(health[k], readHealth(g.Trend, g.State, lag))
		}
	}
	kinds := map[string]string{}
	metas, err := d.clusterMetas(ctx, cluster)
	if err != nil {
		return nil, err
	}
	for _, m := range metas {
		kinds[m.slug] = m.kind
	}
	type key struct{ c, t string }
	parts := map[key]map[uint32]float64{} // partition → produce rate (same for every group)
	lag := map[key]float64{}
	var order []key
	for _, c := range cells {
		k := key{c.cluster, c.topic}
		if parts[k] == nil {
			parts[k] = map[uint32]float64{}
			order = append(order, k)
		}
		parts[k][c.partition] = math.Max(parts[k][c.partition], c.produce)
		lag[k] += c.lag
	}
	prod, err := d.allProducers(ctx, now)
	if err != nil {
		return nil, err
	}
	meta, err := d.topicMeta(ctx, now, cluster)
	if err != nil {
		return nil, err
	}
	for _, tm := range meta { // topics nobody consumes yet
		k := key{tm.cluster, tm.topic}
		if parts[k] == nil {
			parts[k] = map[uint32]float64{}
			order = append(order, k)
		}
	}
	out := []model.Topic{}
	for _, k := range order {
		in := 0.0
		for _, v := range parts[k] {
			in += v
		}
		p := prod[MessagingSystem(kinds[k.c])+" · "+k.t]
		if p == nil {
			p = []string{}
		}
		h := health[k.c+"/"+k.t]
		if h == "" {
			h = model.OK
		}
		t := model.Topic{ClusterID: k.c, Name: k.t, Partitions: len(parts[k]), InPerSec: in, Producers: p, Lag: lag[k], Health: h}
		if tm, ok := meta[k.c+"/"+k.t]; ok {
			if tm.partitions > 0 {
				t.Partitions = int(tm.partitions)
			}
			if tm.inRate >= 0 {
				t.InPerSec = tm.inRate
			}
			t.AvgBytes, t.Replication, t.SizeBytes, t.BytesInPerSec = tm.avgBytes, tm.replication, tm.size, tm.bytesIn
			if t.AvgBytes == 0 && tm.bytesIn > 0 && t.InPerSec > 0 {
				t.AvgBytes = tm.bytesIn / t.InPerSec
			}
			t.Retention = Retention(tm.retentionMs)
			t.RetentionMs = tm.retentionMs
		}
		if t.Partitions == 0 {
			t.Partitions = 1
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InPerSec > out[j].InPerSec })
	return out, nil
}

// MessagingSystem maps a broker kind to the OTel messaging.system used in dependency names.
func MessagingSystem(kind string) string {
	switch kind {
	case "sqs":
		return "aws_sqs"
	case "pubsub":
		return "gcp_pubsub"
	}
	return kind
}

// BrokerKind maps OTel messaging.system to the broker kind / subscription source.
func BrokerKind(system string) string {
	switch strings.ToLower(system) {
	case "aws_sqs", "sqs", "aws.sqs":
		return "sqs"
	case "gcp_pubsub", "pubsub", "gcp.pubsub":
		return "pubsub"
	case "rabbitmq", "amqp":
		return "rabbitmq"
	case "kafka":
		return "kafka"
	}
	return ""
}

// Retention renders retention.ms like the UI ("7d", "12h", "30m"); -1 (Kafka: unlimited) → "∞"; unknown → "".
func Retention(ms float64) string {
	switch {
	case ms < 0 && ms > -2:
		return "∞"
	case ms <= 0:
		return ""
	}
	sec := ms / 1000
	for _, u := range []struct {
		s   float64
		suf string
	}{{86400, "d"}, {3600, "h"}, {60, "m"}, {1, "s"}} {
		if sec >= u.s {
			v := sec / u.s
			if v == math.Trunc(v) {
				return strconv.FormatFloat(v, 'f', 0, 64) + u.suf
			}
			return strconv.FormatFloat(v, 'f', 1, 64) + u.suf
		}
	}
	return strconv.FormatFloat(ms, 'f', 0, 64) + "ms"
}

type topicStat struct {
	cluster, topic                                               string
	partitions, replication, retentionMs, avgBytes, inRate, size float64
	bytesIn                                                      float64
}

// topicMeta reads the latest broker_topics_1m snapshot per topic (last 15 minutes). Unreported values (−Inf) → 0,
// in_rate → −1 so the lag-derived produce rate is used instead.
func (d *DB) topicMeta(ctx context.Context, now time.Time, cluster string) (map[string]topicStat, error) {
	where, args := "", []any{now.Add(-15 * time.Minute).Unix()}
	if cluster != "" {
		where, args = " AND cluster_id = ?", append(args, cluster)
	}
	cols := []string{"partitions", "replication", "retention_ms", "avg_msg_bytes", "in_rate", "size_bytes", "bytes_in_rate"}
	var inner, outer []string
	for _, c := range cols {
		inner = append(inner, fmt.Sprintf("if(countIf(isFinite(%s)) > 0, maxIf(%s, isFinite(%s)), nan) AS %s", c, c, c, c))
		outer = append(outer, fmt.Sprintf("toFloat64(argMaxIf(%s, m, NOT isNaN(%s)))", c, c))
	}
	rows, err := d.ch.Query(ctx, `SELECT cluster_id, topic, `+strings.Join(outer, ", ")+` FROM (
	    SELECT cluster_id, topic, minute AS m, `+strings.Join(inner, ", ")+` FROM broker_topics_1m
	    WHERE {{tenant}} AND minute >= toDateTime(?)`+where+` GROUP BY cluster_id, topic, m)
	  GROUP BY cluster_id, topic`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]topicStat{}
	for rows.Next() {
		var t topicStat
		if err := rows.Scan(&t.cluster, &t.topic, &t.partitions, &t.replication, &t.retentionMs, &t.avgBytes, &t.inRate, &t.size, &t.bytesIn); err != nil {
			return nil, err
		}
		if math.IsNaN(t.inRate) || math.IsInf(t.inRate, 0) {
			t.inRate = -1
		}
		if math.IsNaN(t.retentionMs) || math.IsInf(t.retentionMs, 0) {
			t.retentionMs = 0
		}
		t.partitions, t.replication, t.avgBytes, t.size, t.bytesIn = fin(t.partitions), fin(t.replication), fin(t.avgBytes), fin(t.size), fin(t.bytesIn)
		out[t.cluster+"/"+t.topic] = t
	}
	return out, rows.Err()
}

// allProducers maps queue dependency name ("kafka · orders.created") → producing services.
func (d *DB) allProducers(ctx context.Context, now time.Time) (map[string][]string, error) {
	rows, err := d.ch.Query(ctx, `SELECT dep, groupUniqArray(parent_service) FROM edges_1m WHERE {{tenant}} AND dep_kind = 'queue' AND minute >= toDateTime(?) GROUP BY dep`,
		now.Add(-24*time.Hour).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var dep string
		var s []string
		if err := rows.Scan(&dep, &s); err != nil {
			return nil, err
		}
		sort.Strings(s)
		out[dep] = s
	}
	return out, rows.Err()
}

func buildCluster(m clusterMeta, nodes []model.BrokerNode, groups []model.ConsumerGroup, topics int) model.Cluster {
	c := model.Cluster{
		ID: m.slug, Name: m.name, Kind: m.kind, Endpoint: m.endpoint, Vhost: m.vhost, Source: collectorLabel[m.collector], Collector: m.collector,
		Nodes: nodes, AddedAt: ms(m.created), CredentialsRef: m.creds, Topics: topics,
		Version: m.version, IntegrationID: m.integration, Missing: m.missing,
	}
	if m.source != "" {
		c.Source = m.source
	}
	if len(m.auth) > 0 {
		c.Auth = json.RawMessage(m.auth)
	}
	if c.Nodes == nil {
		c.Nodes = []model.BrokerNode{}
	}
	hs := []model.Health{}
	for _, n := range c.Nodes {
		hs = append(hs, n.Status)
	}
	for _, g := range groups {
		if g.ClusterID == m.slug {
			c.Groups++
			c.TotalLag += g.Lag
			hs = append(hs, g.Health)
		}
	}
	c.Status = worst(hs...)
	return c
}

// ListClusters returns broker clusters with nodes and lag summary.
func (d *DB) ListClusters(ctx context.Context, now time.Time) ([]model.Cluster, error) {
	metas, err := d.clusterMetas(ctx, "")
	if err != nil {
		return nil, err
	}
	nodes, err := d.brokerNodes(ctx, now)
	if err != nil {
		return nil, err
	}
	groups, err := d.ListGroups(ctx, now, GroupFilter{})
	if err != nil {
		return nil, err
	}
	topics, err := d.ListTopics(ctx, now, "")
	if err != nil {
		return nil, err
	}
	tc := map[string]int{}
	for _, t := range topics {
		tc[t.ClusterID]++
	}
	out := make([]model.Cluster, 0, len(metas))
	for _, m := range metas {
		out = append(out, buildCluster(m, nodes[m.slug], groups, tc[m.slug]))
	}
	return out, nil
}

// GetCluster returns one cluster.
func (d *DB) GetCluster(ctx context.Context, now time.Time, slug string) (*model.Cluster, error) {
	metas, err := d.clusterMetas(ctx, slug)
	if err != nil {
		return nil, err
	}
	if len(metas) == 0 {
		return nil, ErrNotFound
	}
	nodes, err := d.brokerNodes(ctx, now)
	if err != nil {
		return nil, err
	}
	groups, err := d.ListGroups(ctx, now, GroupFilter{ClusterID: slug})
	if err != nil {
		return nil, err
	}
	topics, err := d.ListTopics(ctx, now, slug)
	if err != nil {
		return nil, err
	}
	c := buildCluster(metas[0], nodes[slug], groups, len(topics))
	return &c, nil
}

// CreateCluster registers a broker cluster.
func (d *DB) CreateCluster(ctx context.Context, now time.Time, in model.ClusterInput) (*model.Cluster, error) {
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.Endpoint) == "" {
		return nil, Invalid("name and endpoint are required")
	}
	switch in.Kind {
	case "kafka", "rabbitmq", "sqs", "pubsub":
	default:
		return nil, Invalid("kind must be kafka|rabbitmq|sqs|pubsub")
	}
	if in.Collector == "" {
		in.Collector = "agent"
	}
	if collectorLabel[in.Collector] == "" {
		return nil, Invalid("collector must be agent|cloud|direct")
	}
	auth := []byte(in.Auth)
	if len(auth) == 0 || string(auth) == "null" {
		auth = []byte("{}")
	}
	if !json.Valid(auth) {
		return nil, Invalid("auth must be a JSON object")
	}
	slug := LowerSlug(in.ID)
	if slug == "" {
		slug = LowerSlug(in.Name)
	}
	vhost := in.Vhost
	if in.Kind == "rabbitmq" && vhost == "" {
		vhost = "/"
	}
	_, err := d.pg.Exec(ctx, `INSERT INTO broker_clusters (project_id, slug, name, kind, endpoint, vhost, auth, credentials_ref, collector)
	  VALUES ({{project}}, $1, $2, $3, $4, nullif($5, ''), $6, nullif($7, ''), $8)`,
		slug, strings.TrimSpace(in.Name), in.Kind, in.Endpoint, vhost, auth, in.CredentialsRef, in.Collector)
	if isUnique(err) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}
	return d.GetCluster(ctx, now, slug)
}

// DeleteCluster unregisters a broker cluster.
func (d *DB) DeleteCluster(ctx context.Context, slug string) error {
	tag, err := d.pg.Exec(ctx, `DELETE FROM broker_clusters WHERE {{tenant}} AND slug = $1`, slug)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ClusterConn returns kind/endpoint/vhost of a registered cluster (for connection tests).
func (d *DB) ClusterConn(ctx context.Context, slug string) (kind, endpoint, vhost string, err error) {
	metas, err := d.clusterMetas(ctx, slug)
	if err != nil {
		return "", "", "", err
	}
	if len(metas) == 0 {
		return "", "", "", ErrNotFound
	}
	return metas[0].kind, metas[0].endpoint, metas[0].vhost, nil
}

// SetGroupOwner maps a consumer group to a service (manual owner).
func (d *DB) SetGroupOwner(ctx context.Context, cluster, group string, in model.GroupOwnerInput) error {
	if in.ServiceID == "" {
		return Invalid("serviceId is required")
	}
	if _, err := d.pg.Exec(ctx, `INSERT INTO services (project_id, name, type) VALUES ({{project}}, $1, 'worker') ON CONFLICT (project_id, name) DO NOTHING`, in.ServiceID); err != nil {
		return err
	}
	tag, err := d.pg.Exec(ctx, `INSERT INTO consumer_group_owners (cluster_id, consumer_group, service_id, process, source)
	  SELECT c.id, $2, s.id, nullif($4, ''), 'manual' FROM broker_clusters c JOIN services s ON s.project_id = c.project_id AND s.name = $3
	  WHERE {{tenant:c}} AND c.slug = $1
	  ON CONFLICT (cluster_id, consumer_group) DO UPDATE SET service_id = excluded.service_id, process = excluded.process, source = 'manual'`,
		cluster, group, in.ServiceID, in.Process)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GroupLagSeries returns lag per topic (or per partition of one topic) over time; each minute is the latest
// committed state, each bucket the worst minute.
func (d *DB) GroupLagSeries(ctx context.Context, q Q, cluster, group, topic string, byPartition bool) (model.SeriesSet, error) {
	where, args := "", []any{cluster, group, secs(q.W.From), secs(q.W.To)}
	if topic != "" {
		where, args = " AND topic = ?", append(args, topic)
	}
	key := "topic"
	if byPartition {
		key = "concat(topic, '#', toString(partition))"
	}
	rows, err := d.ch.Query(ctx, `SELECT k, `+q.W.BucketExpr("m")+` AS b, toFloat64(max(tl)) FROM (
	    SELECT `+key+` AS k, minute AS m, sum(l) AS tl FROM (
	      SELECT topic, partition, minute, maxIf(lag, isFinite(lag)) AS l FROM broker_lag_1m
	      WHERE {{tenant}} AND cluster_id = ? AND consumer_group = ? AND minute >= toDateTime(?) AND minute < toDateTime(?)`+where+`
	      GROUP BY topic, partition, minute)
	    GROUP BY k, m)
	  GROUP BY k, b ORDER BY k, b`, args...)
	if err != nil {
		return model.SeriesSet{}, err
	}
	defer rows.Close()
	b := timerange.NewBuilder(q.W)
	for rows.Next() {
		var k string
		var bk int64
		var v float64
		if err := rows.Scan(&k, &bk, &v); err != nil {
			return model.SeriesSet{}, err
		}
		b.Set(k, int(bk), v)
	}
	return b.Build(), rows.Err()
}

// TopicSeries returns the produce rate (msgs/s) of a topic and the lag of each group reading it.
func (d *DB) TopicSeries(ctx context.Context, q Q, cluster, topic string) (model.SeriesSet, error) {
	rows, err := d.ch.Query(ctx, `SELECT g, `+q.W.BucketExpr("m")+` AS b, toFloat64(avg(pr)), toFloat64(max(tl)) FROM (
	    SELECT consumer_group AS g, minute AS m, sum(p) AS pr, sum(l) AS tl FROM (
	      SELECT consumer_group, partition, minute, maxIf(produce_rate, isFinite(produce_rate)) AS p, maxIf(lag, isFinite(lag)) AS l FROM broker_lag_1m
	      WHERE {{tenant}} AND cluster_id = ? AND topic = ? AND minute >= toDateTime(?) AND minute < toDateTime(?)
	      GROUP BY consumer_group, partition, minute)
	    GROUP BY g, m)
	  GROUP BY g, b ORDER BY g, b`, cluster, topic, secs(q.W.From), secs(q.W.To))
	if err != nil {
		return model.SeriesSet{}, err
	}
	defer rows.Close()
	b := timerange.NewBuilder(q.W, "in")
	b.Label("in", "Produced / s")
	for rows.Next() {
		var g string
		var bk int64
		var pr, tl float64
		if err := rows.Scan(&g, &bk, &pr, &tl); err != nil {
			return model.SeriesSet{}, err
		}
		if d := b.Data("in"); int(bk) >= 0 && int(bk) < len(d) {
			d[bk] = math.Max(d[bk], fin(pr)) // every group sees the same produce rate
		}
		b.Set("lag:"+g, int(bk), tl)
		b.Label("lag:"+g, g)
	}
	return b.Build(), rows.Err()
}

// BrokerNodeSeries returns one node metric (cpu | memory | disk | net) per node of a cluster.
func (d *DB) BrokerNodeSeries(ctx context.Context, q Q, cluster, metric string) (model.SeriesSet, error) {
	col := map[string]string{"cpu": "cpu", "memory": "memory", "disk": "disk", "net": "net_in"}[metric]
	if col == "" {
		return model.SeriesSet{}, Invalid("metric must be cpu|memory|disk|net")
	}
	rows, err := d.ch.Query(ctx, `SELECT node, `+q.W.BucketExpr("minute")+` AS b, toFloat64(maxIf(`+col+`, isFinite(`+col+`))) FROM broker_nodes_1m
	  WHERE {{tenant}} AND cluster_id = ? AND minute >= toDateTime(?) AND minute < toDateTime(?) GROUP BY node, b ORDER BY node, b`,
		cluster, secs(q.W.From), secs(q.W.To))
	if err != nil {
		return model.SeriesSet{}, err
	}
	defer rows.Close()
	b := timerange.NewBuilder(q.W)
	for rows.Next() {
		var n string
		var bk int64
		var v float64
		if err := rows.Scan(&n, &bk, &v); err != nil {
			return model.SeriesSet{}, err
		}
		b.Set(n, int(bk), v)
	}
	return b.Build(), rows.Err()
}

// ---------------------------------------------------------------- collector

// CollectorConfig lists what the collector should scrape for the key's project.
func (d *DB) CollectorConfig(ctx context.Context) (model.CollectorConfig, error) {
	cfg := model.CollectorConfig{Interval: 15, Databases: []model.CollectorDatabase{}, BrokerClusters: []model.CollectorCluster{}}
	dbs, err := d.dbMetas(ctx, "")
	if err != nil {
		return cfg, err
	}
	for _, m := range dbs {
		cfg.Databases = append(cfg.Databases, model.CollectorDatabase{ID: m.id, Slug: m.slug, Name: m.name, Engine: m.engine, Host: m.host,
			Port: m.port, Collector: m.collector, User: nullable(m.user), CredentialsRef: nullable(m.creds), CloudResourceID: nullable(m.cloud), Env: m.env})
	}
	cls, err := d.clusterMetas(ctx, "")
	if err != nil {
		return cfg, err
	}
	for _, m := range cls {
		auth := json.RawMessage(m.auth)
		if len(auth) == 0 {
			auth = json.RawMessage("{}")
		}
		cfg.BrokerClusters = append(cfg.BrokerClusters, model.CollectorCluster{ID: m.id, Slug: m.slug, Name: m.name, Kind: m.kind,
			Endpoint: m.endpoint, Vhost: nullable(m.vhost), Auth: auth, CredentialsRef: nullable(m.creds), Collector: m.collector})
	}
	return cfg, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// queryKey hashes SQL text so pg_stat_statements text ($1 params) and span text (literals or ?) match:
// $n → ?, lowercase, then normalizeQuery (literals → ?, whitespace collapsed). char(63) is '?' (avoids a bind marker).
func queryKey(expr string) string {
	return `cityHash64(normalizeQuery(lower(replaceRegexpAll(` + expr + `, '\\$[0-9]+', char(63)))))`
}

// dbMetaPublic is the provider metadata trooper-cloud stores in databases.meta (Neon: project, branch, endpoint, state,
// cuMin/cuMax/cuNow, storageGb, storageLimitGb, activeHours; generic keys for other providers). Internal flags and
// anything secret-looking are dropped; always a non-nil object.
func dbMetaPublic(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if k == "missing" || secretConfigKeys[strings.ToLower(k)] {
			continue
		}
		out[k] = v
	}
	return out
}
