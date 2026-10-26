package scrape

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry/services/collector/internal/config"
	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/rate"
)

// Postgres scrapes a PostgreSQL server with a pg_monitor user.
//
//   - connections: client backends in pg_stat_activity; max_connections from settings
//   - ops: Δ(xact_commit + xact_rollback) over all databases / s
//   - hit_ratio: Δblks_hit / Δ(blks_hit + blks_read) (cumulative ratio on the first scrape)
//   - role: pg_is_in_recovery(); replication_lag: on a replica now() − pg_last_xact_replay_timestamp()
//     (0 when caught up), on a primary one series per standby from pg_stat_replication.replay_lag
//   - top queries (pg_stat_statements): Δcalls, Δtotal_exec_time, Δrows per queryid; per-query
//     p95 ≈ mean + 2·stddev (cumulative stats; pg keeps no percentiles)
//   - slow: Δcalls/min of statements whose interval mean ≥ slowQueryMs
//   - latency.p95: calls-weighted p95 of statements' interval mean latencies
//
// CPU / memory / disk are not visible through SQL: they come from the host scraper (agent mode)
// or the cloud integration (CloudWatch / RDS) and are left unset here.
type Postgres struct {
	t    config.Database
	pool *pgxpool.Pool
	log  *slog.Logger
	c    *rate.Counters

	statements     bool // pg_stat_statements usable
	oldColumns     bool // PG ≤ 12: total_time / mean_time / stddev_time
	warnedNoStmts  bool
	lastStmtsCheck time.Time
	lastStmts      time.Time // previous successful pg_stat_statements read
}

// NewPostgres opens a small pool (2 conns) for the target.
func NewPostgres(t config.Database, log *slog.Logger) (*Postgres, error) {
	dsn, err := t.ConnString()
	if err != nil {
		return nil, err
	}
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres dsn: %w", err)
	}
	pc.MaxConns = 2
	pc.MinConns = 0
	pc.MaxConnIdleTime = 5 * time.Minute
	if pc.ConnConfig.RuntimeParams["application_name"] == "" {
		pc.ConnConfig.RuntimeParams["application_name"] = "trooper-agent"
	}
	// statement_timeout guards against a slow pg_stat_statements on huge installations.
	pc.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(t.Timeout.D().Milliseconds(), 10)
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		return nil, err
	}
	return &Postgres{t: t, pool: pool, log: log, c: rate.NewCounters(), statements: true}, nil
}

// Close closes the pool.
func (p *Postgres) Close() error { p.pool.Close(); return nil }

// Scrape implements Scraper.
func (p *Postgres) Scrape(ctx context.Context) (emit.Result, error) {
	var r emit.Result
	now := time.Now()

	var maxConn, conns float64
	var replica bool
	err := p.pool.QueryRow(ctx, `
SELECT current_setting('max_connections')::float8,
       (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend')::float8,
       pg_is_in_recovery()`).Scan(&maxConn, &conns, &replica)
	if err != nil {
		return r, fmt.Errorf("pg_stat_activity: %w", err)
	}
	role := "primary"
	if replica {
		role = "replica"
	}
	base := emit.DBAttrs(p.t.Key(), p.t.DisplayName(), "postgresql", p.t.NodeName(), role)
	r.Gauge(emit.DBConnections, conns, base...)
	r.Gauge(emit.DBMaxConnections, maxConn, base...)

	var xacts, hit, read float64
	err = p.pool.QueryRow(ctx, `
SELECT coalesce(sum(xact_commit + xact_rollback), 0)::float8,
       coalesce(sum(blks_hit), 0)::float8, coalesce(sum(blks_read), 0)::float8
FROM pg_stat_database`).Scan(&xacts, &hit, &read)
	if err != nil {
		return r, fmt.Errorf("pg_stat_database: %w", err)
	}
	if ops, ok := p.c.Rate("xacts", xacts, now); ok {
		r.Gauge(emit.DBOps, ops, base...)
	}
	dh, _, okH := p.c.Delta("blks_hit", hit, now)
	dr, _, okR := p.c.Delta("blks_read", read, now)
	if hr, ok := rate.IntervalHitRatio(dh, dr, okH && okR, hit, read); ok {
		r.Gauge(emit.DBHitRatio, hr, base...)
	}

	if err := p.replication(ctx, replica, base, &r); err != nil {
		p.log.Debug("replication stats unavailable", "target", p.t.Key(), "err", err)
	}
	if err := p.queries(ctx, now, base, &r); err != nil {
		if !p.warnedNoStmts {
			p.log.Warn("pg_stat_statements unavailable; top queries, slow and p95 are not reported "+
				"(CREATE EXTENSION pg_stat_statements; shared_preload_libraries)", "target", p.t.Key(), "err", err)
			p.warnedNoStmts = true
		}
	}
	return r, nil
}

func (p *Postgres) replication(ctx context.Context, replica bool, base []attribute.KeyValue, r *emit.Result) error {
	if replica {
		var lagMs float64
		err := p.pool.QueryRow(ctx, `
SELECT CASE WHEN pg_last_wal_receive_lsn() = pg_last_wal_replay_lsn() THEN 0
            ELSE coalesce(EXTRACT(EPOCH FROM now() - pg_last_xact_replay_timestamp()) * 1000, 0) END::float8`).Scan(&lagMs)
		if err != nil {
			return err
		}
		r.Gauge(emit.DBReplLag, max(lagMs, 0), base...)
		return nil
	}
	rows, err := p.pool.Query(ctx, `
SELECT coalesce(nullif(application_name, ''), host(client_addr), 'standby')::text,
       coalesce(EXTRACT(EPOCH FROM replay_lag) * 1000, 0)::float8
FROM pg_stat_replication`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var node string
		var lag float64
		if err := rows.Scan(&node, &lag); err != nil {
			return err
		}
		a := emit.DBAttrs(p.t.Key(), p.t.DisplayName(), "postgresql", node, "replica")
		r.Gauge(emit.DBReplLag, lag, a...)
	}
	return rows.Err()
}

type pgStmt struct {
	id                      int64
	text                    string
	calls, totalMs, rows    float64
	meanMs, stddevMs        float64
	dCalls, dTotalMs, dRows float64
	haveDelta               bool
}

const pgStmtsNew = `
SELECT queryid, query, sum(calls)::float8, sum(total_exec_time)::float8, sum(rows)::float8,
       avg(mean_exec_time)::float8, max(stddev_exec_time)::float8
FROM pg_stat_statements WHERE queryid IS NOT NULL GROUP BY queryid, query`

const pgStmtsOld = `
SELECT queryid, query, sum(calls)::float8, sum(total_time)::float8, sum(rows)::float8,
       avg(mean_time)::float8, max(stddev_time)::float8
FROM pg_stat_statements WHERE queryid IS NOT NULL GROUP BY queryid, query`

func (p *Postgres) queries(ctx context.Context, now time.Time, base []attribute.KeyValue, r *emit.Result) error {
	if !p.statements && now.Sub(p.lastStmtsCheck) < 10*time.Minute {
		return errors.New("pg_stat_statements disabled (rechecking every 10 min)")
	}
	p.lastStmtsCheck = now
	q := pgStmtsNew
	if p.oldColumns {
		q = pgStmtsOld
	}
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42703" && !p.oldColumns { // undefined_column: PG ≤ 12
			p.oldColumns = true
			return p.queries(ctx, now, base, r)
		}
		p.statements = false
		return err
	}
	stmts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pgStmt, error) {
		var s pgStmt
		err := row.Scan(&s.id, &s.text, &s.calls, &s.totalMs, &s.rows, &s.meanMs, &s.stddevMs)
		return s, err
	})
	if err != nil {
		p.statements = false
		return err
	}
	p.statements = true
	p.warnedNoStmts = false

	weighted := make([]rate.Weighted, 0, len(stmts))
	var slowCalls float64
	since := p.lastStmts
	p.lastStmts = now
	byDelta := make([]float64, len(stmts))
	for i := range stmts {
		s := &stmts[i]
		k := strconv.FormatInt(s.id, 10)
		var ok bool
		s.dCalls, ok = p.c.NewKeyDelta("calls/"+k, s.calls, now, since)
		s.dTotalMs, _ = p.c.NewKeyDelta("time/"+k, s.totalMs, now, since)
		s.dRows, _ = p.c.NewKeyDelta("rows/"+k, s.rows, now, since)
		if !ok || s.dCalls <= 0 {
			continue
		}
		mean := s.dTotalMs / s.dCalls
		weighted = append(weighted, rate.Weighted{Value: mean, Weight: s.dCalls})
		if mean >= p.t.SlowQueryMs {
			slowCalls += s.dCalls
		}
		byDelta[i] = s.dTotalMs + 1e-9*s.dCalls // rank by DB time, calls break ties
	}
	p.c.Prune(now.Add(-10 * p.t.Interval.D()))
	if since.IsZero() {
		return nil // first scrape: baseline only
	}
	r.Gauge(emit.DBSlow, rate.PerMinute(slowCalls, now.Sub(since)), base...)
	if p95, ok := rate.WeightedQuantile(weighted, 0.95); ok {
		r.Gauge(emit.DBLatencyP95, p95, base...)
	}
	for _, i := range TopN(byDelta, p.t.TopQueries) {
		s := stmts[i]
		a := []attribute.KeyValue{
			attribute.String(emit.AttrDBID, p.t.Key()),
			attribute.String(emit.AttrQueryHash, strconv.FormatUint(uint64(s.id), 10)),
			attribute.String(emit.AttrQueryText, NormalizeQueryText(s.text)),
		}
		r.Count(emit.DBQueryCalls, s.dCalls, a...)
		r.Count(emit.DBQueryTotalMs, s.dTotalMs, a...)
		r.Count(emit.DBQueryRows, s.dRows, a...)
		r.Gauge(emit.DBQueryP95Ms, rate.ApproxP95(s.meanMs, s.stddevMs), a...)
	}
	return nil
}
