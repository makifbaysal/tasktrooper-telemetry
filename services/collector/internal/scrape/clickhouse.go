package scrape

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry/services/collector/internal/config"
	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/rate"
)

// ClickHouse scrapes a ClickHouse server over the native protocol (clickhouse-go v2) with a user
// that can read the system tables (SELECT ON system.*; see README § Monitoring users).
//
//   - connections: system.metrics TCP + HTTP + MySQL + PostgreSQL + Interserver connections;
//     max_connections: system.server_settings
//   - ops: ΔQuery / s (system.events); errors (custom, /min): ΔFailedQuery
//   - hit_ratio: ΔMarkCacheHits / Δ(hits + misses)
//   - cpu / memory: system.asynchronous_metrics (OS*TimeNormalized, MemoryResident / CGroup or OS total);
//     disk: 1 − Σfree / Σtotal of system.disks
//   - replication_lag: max(system.replicas.absolute_delay) × 1000, only when replicated tables exist
//   - latency.p95, slow, top queries: system.query_log in a server-time window (cursor, now − flush lag],
//     grouped by normalized_query_hash; the agent's own statements carry a comment marker and are
//     excluded
//
// Role is always "primary" (ClickHouse replicas are multi-leader).
type ClickHouse struct {
	t   config.Database
	src chSource
	log *slog.Logger
	c   *rate.Counters
	now func() time.Time

	cursor      int64 // server unix seconds; query_log rows up to here are reported
	noQueryLog  bool
	noSettings  bool
	warnedQLog  bool
	warnedParts bool
}

// chMarker prefixes every statement the agent sends, so query_log rows of the agent are skipped.
const chMarker = "/* trooper-agent */ "

// chFlushLag keeps the query_log window behind server time: rows are flushed every
// flush_interval_milliseconds (7.5 s by default).
const chFlushLag = 10

// Queries (constants so the fake source can match them).
const (
	chqMetrics = chMarker + `SELECT metric, toFloat64(value) FROM system.metrics WHERE metric IN
 ('TCPConnection','HTTPConnection','MySQLConnection','PostgreSQLConnection','InterserverConnection')`
	chqEvents = chMarker + `SELECT event, toFloat64(value) FROM system.events WHERE event IN
 ('Query','FailedQuery','MarkCacheHits','MarkCacheMisses')`
	chqAsync = chMarker + `SELECT metric, toFloat64(value) FROM system.asynchronous_metrics WHERE metric IN
 ('OSUserTimeNormalized','OSSystemTimeNormalized','MemoryResident','OSMemoryTotal','CGroupMemoryTotal')`
	chqMaxConn = chMarker + `SELECT 'max_connections', toFloat64OrZero(value) FROM system.server_settings WHERE name = 'max_connections'`
	chqDisks   = chMarker + `SELECT 'total', toFloat64(sum(total_space)) FROM system.disks UNION ALL
SELECT 'free', toFloat64(sum(free_space)) FROM system.disks`
	chqReplicas = chMarker + `SELECT 'replicas', toFloat64(count()) FROM system.replicas UNION ALL
SELECT 'delay', toFloat64(max(absolute_delay)) FROM system.replicas`
	chqNow = chMarker + `SELECT 'now', toFloat64(toUnixTimestamp(now()))`

	chqWhere = ` FROM system.query_log
WHERE event_date >= toDate(fromUnixTimestamp(?)) AND event_time > fromUnixTimestamp(?) AND event_time <= fromUnixTimestamp(?)
  AND type IN ('QueryFinish', 'ExceptionWhileProcessing') AND is_initial_query
  AND NOT startsWith(query, '` + chMarker + `')`
	chqWindow = chMarker + `SELECT 'p95', toFloat64(quantile(0.95)(query_duration_ms))` + chqWhere + `
UNION ALL SELECT 'calls', toFloat64(count())` + chqWhere + `
UNION ALL SELECT 'slow', toFloat64(countIf(query_duration_ms >= ?))` + chqWhere
	chqTop = chMarker + `SELECT toString(normalized_query_hash), any(normalizeQuery(query)), toFloat64(count()),
 toFloat64(sum(query_duration_ms)), toFloat64(sum(if(query_kind = 'Insert', written_rows, result_rows))),
 toFloat64(quantile(0.95)(query_duration_ms))` + chqWhere + `
GROUP BY normalized_query_hash`
)

// chStmt is one normalized statement's interval totals from system.query_log.
type chStmt struct {
	hash, text            string
	calls, totalMs, rowsN float64
	p95Ms                 float64
}

// chSource is the query layer (database/sql in production, a fake in tests).
type chSource interface {
	// pairs runs a (String, Float64) query.
	pairs(ctx context.Context, q string, args ...any) (map[string]float64, error)
	// statements runs chqTop.
	statements(ctx context.Context, q string, args ...any) ([]chStmt, error)
	Close() error
}

type chSQL struct{ db *sql.DB }

func (s chSQL) pairs(ctx context.Context, q string, args ...any) (map[string]float64, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var k string
		var v float64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] += v
	}
	return out, rows.Err()
}

func (s chSQL) statements(ctx context.Context, q string, args ...any) ([]chStmt, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chStmt
	for rows.Next() {
		var st chStmt
		if err := rows.Scan(&st.hash, &st.text, &st.calls, &st.totalMs, &st.rowsN, &st.p95Ms); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s chSQL) Close() error { return s.db.Close() }

// NewClickHouse opens the target.
func NewClickHouse(t config.Database, log *slog.Logger) (*ClickHouse, error) {
	dsn, err := t.ConnString()
	if err != nil {
		return nil, err
	}
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("clickhouse dsn: %w", err)
	}
	opts.DialTimeout = t.Timeout.D()
	opts.ReadTimeout = t.Timeout.D()
	opts.ClientInfo.Products = append(opts.ClientInfo.Products, struct{ Name, Version string }{"trooper-agent", "1"})
	db := clickhouse.OpenDB(opts)
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return newClickHouse(t, chSQL{db: db}, log), nil
}

func newClickHouse(t config.Database, src chSource, log *slog.Logger) *ClickHouse {
	return &ClickHouse{t: t, src: src, log: log, c: rate.NewCounters(), now: time.Now}
}

// Close closes the pool.
func (c *ClickHouse) Close() error { return c.src.Close() }

// Scrape implements Scraper.
func (c *ClickHouse) Scrape(ctx context.Context) (emit.Result, error) {
	var r emit.Result
	now := c.now()

	metrics, err := c.src.pairs(ctx, chqMetrics)
	if err != nil {
		return r, fmt.Errorf("system.metrics: %w", err)
	}
	events, err := c.src.pairs(ctx, chqEvents)
	if err != nil {
		return r, fmt.Errorf("system.events: %w", err)
	}
	base := emit.DBAttrs(c.t.Key(), c.t.DisplayName(), "clickhouse", c.t.NodeName(), "primary")

	var conns float64
	for _, v := range metrics {
		conns += v
	}
	r.Gauge(emit.DBConnections, conns, base...)
	if !c.noSettings {
		// system.server_settings exists since 23.3
		if s, err := c.src.pairs(ctx, chqMaxConn); err != nil {
			c.noSettings = true
		} else if v := s["max_connections"]; v > 0 {
			r.Gauge(emit.DBMaxConnections, v, base...)
		}
	}

	if ops, ok := c.c.Rate("query", events["Query"], now); ok {
		r.Gauge(emit.DBOps, ops, base...)
	}
	if d, dt, ok := c.c.Delta("failed", events["FailedQuery"], now); ok {
		r.Gauge(emit.DBErrors, rate.PerMinute(d, dt), base...)
	}
	hits, misses := events["MarkCacheHits"], events["MarkCacheMisses"]
	dh, _, ok1 := c.c.Delta("mark_hits", hits, now)
	dm, _, ok2 := c.c.Delta("mark_misses", misses, now)
	if hr, ok := rate.IntervalHitRatio(dh, dm, ok1 && ok2, hits, misses); ok {
		r.Gauge(emit.DBHitRatio, hr, base...)
	}

	if a, err := c.src.pairs(ctx, chqAsync); err == nil {
		chResources(a, base, &r)
	}
	if d, err := c.src.pairs(ctx, chqDisks); err == nil {
		if v, ok := rate.Fraction(d["total"]-d["free"], d["total"]); ok {
			r.Gauge(emit.DBDisk, v, base...)
		}
	}
	if rep, err := c.src.pairs(ctx, chqReplicas); err != nil {
		if !c.warnedParts {
			c.warnedParts = true
			c.log.Warn("system.replicas unreadable; replication lag is not reported", "target", c.t.Key(), "err", err)
		}
	} else if rep["replicas"] > 0 {
		r.Gauge(emit.DBReplLag, rep["delay"]*1000, base...)
	}

	if err := c.queries(ctx, base, &r); err != nil && !c.warnedQLog {
		c.warnedQLog = true
		c.log.Warn("system.query_log unavailable; latency, slow and top queries are not reported", "target", c.t.Key(), "err", err)
	}
	return r, nil
}

// chResources turns asynchronous_metrics into cpu / memory (0..1).
func chResources(a map[string]float64, base []attribute.KeyValue, r *emit.Result) {
	if u, ok := a["OSUserTimeNormalized"]; ok {
		r.Gauge(emit.DBCPU, rate.Clamp01(u+a["OSSystemTimeNormalized"]), base...)
	}
	total := a["OSMemoryTotal"]
	if cg := a["CGroupMemoryTotal"]; cg > 0 && (total == 0 || cg < total) {
		total = cg
	}
	if v, ok := rate.Fraction(a["MemoryResident"], total); ok {
		r.Gauge(emit.DBMemory, v, base...)
	}
}

// queries reads system.query_log rows in (cursor, server now − chFlushLag]. The first scrape only
// sets the cursor; after a long gap the window is capped to one interval.
func (c *ClickHouse) queries(ctx context.Context, base []attribute.KeyValue, r *emit.Result) error {
	if c.noQueryLog {
		return nil
	}
	n, err := c.src.pairs(ctx, chqNow)
	if err != nil {
		return err
	}
	hi := int64(n["now"]) - chFlushLag
	lo := c.cursor
	step := int64(max(c.t.Interval.D(), time.Second) / time.Second)
	if lo == 0 || hi-lo > 10*step {
		c.cursor = hi
		if lo == 0 {
			return nil
		}
		lo = hi - step
	}
	if hi <= lo {
		return nil
	}
	win := []any{lo, lo, hi}
	args := append(append(append(append([]any{}, win...), win...), c.t.SlowQueryMs), win...)
	w, err := c.src.pairs(ctx, chqWindow, args...)
	if err != nil {
		c.noQueryLog = true
		return err
	}
	c.cursor = hi
	minutes := float64(hi-lo) / 60
	r.Gauge(emit.DBSlow, w["slow"]/minutes, base...)
	if w["calls"] > 0 {
		r.Gauge(emit.DBLatencyP95, w["p95"], base...)
	}
	stmts, err := c.src.statements(ctx, chqTop, win...)
	if err != nil {
		return err
	}
	rank := make([]float64, len(stmts))
	for i, s := range stmts {
		rank[i] = s.totalMs + 1e-9*s.calls
	}
	for _, i := range TopN(rank, c.t.TopQueries) {
		s := stmts[i]
		a := []attribute.KeyValue{
			attribute.String(emit.AttrDBID, c.t.Key()),
			attribute.String(emit.AttrQueryHash, s.hash),
			attribute.String(emit.AttrQueryText, NormalizeQueryText(strings.TrimPrefix(s.text, chMarker))),
		}
		r.Count(emit.DBQueryCalls, s.calls, a...)
		r.Count(emit.DBQueryTotalMs, s.totalMs, a...)
		r.Count(emit.DBQueryRows, s.rowsN, a...)
		r.Gauge(emit.DBQueryP95Ms, s.p95Ms, a...)
	}
	return nil
}
