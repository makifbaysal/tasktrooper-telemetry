package scrape

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry/services/collector/internal/config"
	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/rate"
)

// MySQL scrapes MySQL / MariaDB with a PROCESS, REPLICATION CLIENT, SELECT ON performance_schema user.
//
//   - connections: Threads_connected / @@max_connections
//   - ops: ΔQuestions / s; slow: ΔSlow_queries / min
//   - hit_ratio: 1 − ΔInnodb_buffer_pool_reads / ΔInnodb_buffer_pool_read_requests
//   - role/replication_lag: SHOW REPLICA STATUS (fallback SHOW SLAVE STATUS), Seconds_Behind_* × 1000
//   - top queries: performance_schema.events_statements_summary_by_digest deltas
//     (COUNT_STAR, SUM_TIMER_WAIT ps→ms, SUM_ROWS_SENT); per-query p95 = QUANTILE_95 (8.0+), else AVG
//   - server-side prepared statements (binary protocol / PREPARE) are not in the digest summary:
//     performance_schema.prepared_statements_instances deltas (COUNT_EXECUTE, SUM_TIMER_EXECUTE,
//     SUM_ROWS_SENT) per (thread, statement), merged into the digest of their text
//     (STATEMENT_DIGEST, 8.0+, with ? → 0 so the parser accepts it; the result equals the
//     text-protocol digest). Only statements still allocated at scrape time are seen.
//   - latency.p95: calls-weighted p95 of digests' interval mean latencies
//
// CPU / memory / disk: host scraper or cloud integration, as for Postgres.
type MySQL struct {
	t   config.Database
	db  *sql.DB
	log *slog.Logger
	c   *rate.Counters

	noQuantile   bool
	noDigests    bool
	noPrepared   bool
	psDigest     map[string][2]string // prepared SQL_TEXT → (digest, digest text)
	warnedDigest bool
	lastDigests  time.Time
}

// NewMySQL opens the target.
func NewMySQL(t config.Database, log *slog.Logger) (*MySQL, error) {
	dsn, err := t.ConnString()
	if err != nil {
		return nil, err
	}
	dsn = strings.TrimPrefix(dsn, "mysql://")
	mc, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("mysql dsn: %w", err)
	}
	mc.Timeout = t.Timeout.D()
	mc.ReadTimeout = t.Timeout.D()
	connector, err := mysql.NewConnector(mc)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return &MySQL{t: t, db: db, log: log, c: rate.NewCounters()}, nil
}

// Close closes the pool.
func (m *MySQL) Close() error { return m.db.Close() }

// Scrape implements Scraper.
func (m *MySQL) Scrape(ctx context.Context) (emit.Result, error) {
	var r emit.Result
	now := time.Now()

	status := map[string]float64{}
	rows, err := m.db.QueryContext(ctx, `SHOW GLOBAL STATUS WHERE Variable_name IN
 ('Threads_connected','Questions','Slow_queries','Innodb_buffer_pool_read_requests','Innodb_buffer_pool_reads')`)
	if err != nil {
		return r, fmt.Errorf("global status: %w", err)
	}
	for rows.Next() {
		var k string
		var v float64
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return r, err
		}
		status[k] = v
	}
	rows.Close()
	var maxConn float64
	if err := m.db.QueryRowContext(ctx, `SELECT @@max_connections`).Scan(&maxConn); err != nil {
		return r, fmt.Errorf("max_connections: %w", err)
	}

	role, lagMs, haveLag := m.replica(ctx)
	base := emit.DBAttrs(m.t.Key(), m.t.DisplayName(), "mysql", m.t.NodeName(), role)
	r.Gauge(emit.DBConnections, status["Threads_connected"], base...)
	r.Gauge(emit.DBMaxConnections, maxConn, base...)
	if haveLag {
		r.Gauge(emit.DBReplLag, lagMs, base...)
	}
	if ops, ok := m.c.Rate("questions", status["Questions"], now); ok {
		r.Gauge(emit.DBOps, ops, base...)
	}
	if d, dt, ok := m.c.Delta("slow", status["Slow_queries"], now); ok {
		r.Gauge(emit.DBSlow, rate.PerMinute(d, dt), base...)
	}
	req, reads := status["Innodb_buffer_pool_read_requests"], status["Innodb_buffer_pool_reads"]
	dReq, _, ok1 := m.c.Delta("bp_req", req, now)
	dReads, _, ok2 := m.c.Delta("bp_reads", reads, now)
	// read_requests counts logical reads (hits + misses); reads counts misses from disk.
	if hr, ok := rate.IntervalHitRatio(dReq-dReads, dReads, ok1 && ok2, req-reads, reads); ok {
		r.Gauge(emit.DBHitRatio, hr, base...)
	}

	if err := m.queries(ctx, now, base, &r); err != nil && !m.warnedDigest {
		m.warnedDigest = true
		m.log.Warn("performance_schema digests unavailable; top queries and p95 are not reported", "target", m.t.Key(), "err", err)
	}
	return r, nil
}

// replica returns role and lag. SHOW REPLICA STATUS needs 8.0.22+; older servers / MariaDB use
// SHOW SLAVE STATUS.
func (m *MySQL) replica(ctx context.Context) (role string, lagMs float64, ok bool) {
	for _, q := range []string{"SHOW REPLICA STATUS", "SHOW SLAVE STATUS"} {
		rows, err := m.db.QueryContext(ctx, q)
		if err != nil {
			continue
		}
		cols, _ := rows.Columns()
		if !rows.Next() {
			rows.Close()
			return "primary", 0, false
		}
		vals := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			rows.Close()
			return "replica", 0, false
		}
		var sec float64
		found := false
		for i, c := range cols {
			if (c == "Seconds_Behind_Source" || c == "Seconds_Behind_Master") && vals[i] != nil {
				if _, err := fmt.Sscan(string(vals[i]), &sec); err == nil {
					found = true
				}
			}
		}
		rows.Close()
		return "replica", sec * 1000, found
	}
	return "primary", 0, false
}

type delta struct {
	d                     *myDigest
	calls, totalMs, rowsN float64
}

type myDigest struct {
	digest, text         string
	calls, timerPs, rows float64
	p95Ps                float64
}

func (m *MySQL) queries(ctx context.Context, now time.Time, base []attribute.KeyValue, r *emit.Result) error {
	if m.noDigests {
		return nil
	}
	q := `SELECT DIGEST, coalesce(DIGEST_TEXT, ''), COUNT_STAR, SUM_TIMER_WAIT, SUM_ROWS_SENT, QUANTILE_95
FROM performance_schema.events_statements_summary_by_digest WHERE DIGEST IS NOT NULL`
	if m.noQuantile {
		q = strings.Replace(q, "QUANTILE_95", "AVG_TIMER_WAIT", 1)
	}
	rows, err := m.db.QueryContext(ctx, q)
	if err != nil {
		var me *mysql.MySQLError
		if !m.noQuantile && errors.As(err, &me) && me.Number == 1054 { // unknown column
			m.noQuantile = true
			return m.queries(ctx, now, base, r)
		}
		m.noDigests = true
		return err
	}
	defer rows.Close()
	// The same digest appears once per schema: merge.
	merged := map[string]*myDigest{}
	var order []string
	for rows.Next() {
		var d myDigest
		if err := rows.Scan(&d.digest, &d.text, &d.calls, &d.timerPs, &d.rows, &d.p95Ps); err != nil {
			return err
		}
		if x, ok := merged[d.digest]; ok {
			x.calls += d.calls
			x.timerPs += d.timerPs
			x.rows += d.rows
			x.p95Ps = max(x.p95Ps, d.p95Ps)
			continue
		}
		merged[d.digest] = &d
		order = append(order, d.digest)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	since := m.lastDigests
	m.lastDigests = now
	deltas := make([]delta, 0, len(order))
	rank := make([]float64, 0, len(order))
	var weighted []rate.Weighted
	for _, k := range order {
		d := merged[k]
		dc, ok := m.c.NewKeyDelta("calls/"+k, d.calls, now, since)
		dtm, _ := m.c.NewKeyDelta("time/"+k, d.timerPs, now, since)
		dr, _ := m.c.NewKeyDelta("rows/"+k, d.rows, now, since)
		if !ok || dc <= 0 {
			continue
		}
		ms := dtm / 1e9 // picoseconds → ms
		deltas = append(deltas, delta{d: d, calls: dc, totalMs: ms, rowsN: dr})
		rank = append(rank, ms+1e-9*dc)
		weighted = append(weighted, rate.Weighted{Value: ms / dc, Weight: dc})
	}
	for _, pd := range m.prepared(ctx, now, since) {
		merged := false
		for i := range deltas {
			if deltas[i].d.digest == pd.d.digest {
				deltas[i].calls += pd.calls
				deltas[i].totalMs += pd.totalMs
				deltas[i].rowsN += pd.rowsN
				rank[i] = deltas[i].totalMs + 1e-9*deltas[i].calls
				deltas[i].d.p95Ps = max(deltas[i].d.p95Ps, pd.d.p95Ps)
				merged = true
				break
			}
		}
		if !merged {
			deltas = append(deltas, delta{d: pd.d, calls: pd.calls, totalMs: pd.totalMs, rowsN: pd.rowsN})
			rank = append(rank, pd.totalMs+1e-9*pd.calls)
		}
		weighted = append(weighted, rate.Weighted{Value: pd.totalMs / pd.calls, Weight: pd.calls})
	}
	m.c.Prune(now.Add(-10 * m.t.Interval.D()))
	if p95, ok := rate.WeightedQuantile(weighted, 0.95); ok {
		r.Gauge(emit.DBLatencyP95, p95, base...)
	}
	for _, i := range TopN(rank, m.t.TopQueries) {
		d := deltas[i]
		a := []attribute.KeyValue{
			attribute.String(emit.AttrDBID, m.t.Key()),
			attribute.String(emit.AttrQueryHash, HexDigestToUint64(d.d.digest)),
			attribute.String(emit.AttrQueryText, NormalizeQueryText(d.d.text)),
		}
		r.Count(emit.DBQueryCalls, d.calls, a...)
		r.Count(emit.DBQueryTotalMs, d.totalMs, a...)
		r.Count(emit.DBQueryRows, d.rowsN, a...)
		r.Gauge(emit.DBQueryP95Ms, d.d.p95Ps/1e9, a...)
	}
	return nil
}

// prepared returns the interval deltas of server-side prepared statements, merged per digest.
// Per-statement p95 is approximated by the interval mean (the table has no quantiles).
func (m *MySQL) prepared(ctx context.Context, now, since time.Time) []delta {
	if m.noPrepared {
		return nil
	}
	rows, err := m.db.QueryContext(ctx, `SELECT OWNER_THREAD_ID, STATEMENT_ID, SQL_TEXT, COUNT_EXECUTE, SUM_TIMER_EXECUTE, SUM_ROWS_SENT
FROM performance_schema.prepared_statements_instances WHERE COUNT_EXECUTE > 0`)
	if err != nil {
		m.noPrepared = true
		m.log.Warn("performance_schema.prepared_statements_instances unavailable; prepared statements are not in top queries", "target", m.t.Key(), "err", err)
		return nil
	}
	type inst struct {
		thread, id       int64
		text             string
		calls, ps, rowsN float64
	}
	var all []inst
	for rows.Next() {
		var i inst
		var thread sql.NullInt64
		if err := rows.Scan(&thread, &i.id, &i.text, &i.calls, &i.ps, &i.rowsN); err != nil {
			rows.Close()
			return nil
		}
		i.thread = thread.Int64
		all = append(all, i)
	}
	rows.Close()
	if m.psDigest == nil {
		m.psDigest = map[string][2]string{}
	}
	byDigest := map[string]*delta{}
	var order []string
	for _, i := range all {
		key := fmt.Sprintf("ps/%d/%d", i.thread, i.id)
		dc, ok := m.c.NewKeyDelta("calls/"+key, i.calls, now, since)
		dt, _ := m.c.NewKeyDelta("time/"+key, i.ps, now, since)
		dr, _ := m.c.NewKeyDelta("rows/"+key, i.rowsN, now, since)
		if !ok || dc <= 0 {
			continue
		}
		dg, ok := m.psDigest[i.text]
		if !ok {
			dg = m.digestOf(ctx, i.text)
			if len(m.psDigest) > 10000 {
				clear(m.psDigest)
			}
			m.psDigest[i.text] = dg
		}
		d := byDigest[dg[0]]
		if d == nil {
			d = &delta{d: &myDigest{digest: dg[0], text: dg[1]}}
			byDigest[dg[0]] = d
			order = append(order, dg[0])
		}
		d.calls += dc
		d.totalMs += dt / 1e9
		d.rowsN += dr
	}
	out := make([]delta, 0, len(order))
	for _, k := range order {
		d := byDigest[k]
		d.d.p95Ps = d.totalMs / d.calls * 1e9
		out = append(out, *d)
	}
	return out
}

// digestOf returns MySQL's digest + digest text of a prepared statement's SQL; on servers without
// STATEMENT_DIGEST (MariaDB, 5.7) or unparsable text: FNV of the normalized text (hex, so it goes
// through HexDigestToUint64 like real digests).
func (m *MySQL) digestOf(ctx context.Context, text string) [2]string {
	var dg, dt string
	err := m.db.QueryRowContext(ctx, `SELECT STATEMENT_DIGEST(REPLACE(?, CHAR(63), '0')), STATEMENT_DIGEST_TEXT(REPLACE(?, CHAR(63), '0'))`, text, text).Scan(&dg, &dt)
	if err == nil && dg != "" {
		return [2]string{dg, dt}
	}
	n := NormalizeQueryText(text)
	h, _ := strconv.ParseUint(HashText(n), 10, 64)
	return [2]string{fmt.Sprintf("%016x", h), n}
}
