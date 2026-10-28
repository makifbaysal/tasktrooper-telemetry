// Package store is the Postgres access of loadtest-ctl and loadtest-runner (tables of 001_init.sql and
// 006_loadtest.sql).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps a pool.
type Store struct{ pg *pgxpool.Pool }

// Open connects.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pg: pool}, nil
}

// Close closes the pool.
func (s *Store) Close() { s.pg.Close() }

// Pool exposes the pool (tests).
func (s *Store) Pool() *pgxpool.Pool { return s.pg }

// Run is a load test run with its scenario.
type Run struct {
	ID          int64
	ScenarioID  int64
	ProjectID   int64
	Spec        []byte
	State       string
	StartedAt   time.Time
	RequestedBy *int64
	Abort       bool // abort_requested_at set (or the api already set state 'aborted')
	VUMinutes   float64
}

// ShardInit is a region runner the controller is about to start.
type ShardInit struct {
	Region string
	Share  float64
}

// Admission is the controller's verdict on a claimed run.
type Admission struct {
	Reject    string // non-empty: state 'error' with this message
	Shards    []ShardInit
	VUMinutes float64 // reserved in loadtest_usage
}

// ErrNone means there was no queued run.
var ErrNone = errors.New("no queued run")

// ClaimNext locks the oldest queued run (FOR UPDATE SKIP LOCKED), asks admit for a verdict and either moves
// it to 'running' (shards created, usage reserved) or to 'error'. It returns the run as claimed.
func (s *Store) ClaimNext(ctx context.Context, instance string, projects []int64, admit func(Run) Admission) (*Run, *Admission, error) {
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var r Run
	err = tx.QueryRow(ctx, `SELECT r.id, r.scenario_id, s.project_id, s.spec, r.requested_by
	  FROM loadtest_runs r JOIN loadtest_scenarios s ON s.id = r.scenario_id
	  WHERE r.state = 'queued' AND r.abort_requested_at IS NULL AND (cardinality($1::bigint[]) = 0 OR s.project_id = ANY($1))
	  ORDER BY r.id FOR UPDATE OF r SKIP LOCKED LIMIT 1`, projectList(projects)).Scan(&r.ID, &r.ScenarioID, &r.ProjectID, &r.Spec, &r.RequestedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNone
	}
	if err != nil {
		return nil, nil, err
	}
	a := admit(r)
	if a.Reject != "" {
		r.State = "error"
		if _, err := tx.Exec(ctx, `UPDATE loadtest_runs SET state = 'error', error = $2, finished_at = now(), claimed_by = $3,
		  summary = coalesce(summary, '{}'::jsonb) || jsonb_build_object('error', $2::text) WHERE id = $1`, r.ID, a.Reject, instance); err != nil {
			return nil, nil, err
		}
		return &r, &a, tx.Commit(ctx)
	}
	regions := make([]string, len(a.Shards))
	for i, sh := range a.Shards {
		regions[i] = sh.Region
		if _, err := tx.Exec(ctx, `INSERT INTO loadtest_run_shards (run_id, region, share) VALUES ($1, $2, $3)
		  ON CONFLICT (run_id, region) DO UPDATE SET share = EXCLUDED.share, state = 'pending', live = NULL, stats = NULL,
		  summary = NULL, error = NULL, abort_reason = NULL, started_at = NULL, finished_at = NULL, updated_at = now()`,
			r.ID, sh.Region, sh.Share); err != nil {
			return nil, nil, err
		}
	}
	if err := tx.QueryRow(ctx, `UPDATE loadtest_runs SET state = 'running', started_at = now(), finished_at = NULL, claimed_by = $2,
	  region = $3, vu_minutes = $4, error = NULL, live = NULL, summary = '{"progress": 0}'::jsonb, thresholds_passed = NULL
	  WHERE id = $1 RETURNING started_at`, r.ID, instance, strings.Join(regions, ","), a.VUMinutes).Scan(&r.StartedAt); err != nil {
		return nil, nil, err
	}
	if a.VUMinutes > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO loadtest_usage (project_id, month, vu_minutes)
		  VALUES ($1, date_trunc('month', now() AT TIME ZONE 'UTC')::date, $2)
		  ON CONFLICT (project_id, month) DO UPDATE SET vu_minutes = loadtest_usage.vu_minutes + EXCLUDED.vu_minutes`,
			r.ProjectID, a.VUMinutes); err != nil {
			return nil, nil, err
		}
	}
	r.State, r.VUMinutes = "running", a.VUMinutes
	return &r, &a, tx.Commit(ctx)
}

// Domain is a load test target domain.
type Domain struct {
	ID        int64
	ProjectID int64
	Host      string
	Token     string
	Method    string
	Verified  bool
}

// VerifiedDomains lists a project's verified hosts.
func (s *Store) VerifiedDomains(ctx context.Context, project int64) ([]Domain, error) {
	rows, err := s.pg.Query(ctx, `SELECT id, project_id, lower(host), token, coalesce(method, '') FROM loadtest_domains
	  WHERE project_id = $1 AND verified_at IS NOT NULL`, project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Domain, error) {
		d := Domain{Verified: true}
		return d, r.Scan(&d.ID, &d.ProjectID, &d.Host, &d.Token, &d.Method)
	})
}

// UsageThisMonth returns the project's VU-minutes of the current UTC month.
func (s *Store) UsageThisMonth(ctx context.Context, project int64) (float64, error) {
	var v float64
	err := s.pg.QueryRow(ctx, `SELECT coalesce(sum(vu_minutes), 0)::float8 FROM loadtest_usage
	  WHERE project_id = $1 AND month = date_trunc('month', now() AT TIME ZONE 'UTC')::date`, project).Scan(&v)
	return v, err
}

// PendingVerifications are unverified domains with a verification request not checked since, plus a retry
// every minute for 15 minutes after the request (DNS propagation).
func (s *Store) PendingVerifications(ctx context.Context, projects []int64) ([]Domain, error) {
	rows, err := s.pg.Query(ctx, `SELECT id, project_id, lower(host), token, '' FROM loadtest_domains
	  WHERE verified_at IS NULL AND verify_requested_at IS NOT NULL
	    AND (cardinality($1::bigint[]) = 0 OR project_id = ANY($1))
	    AND (last_checked_at IS NULL OR last_checked_at < verify_requested_at
	         OR (last_checked_at < now() - interval '1 minute' AND verify_requested_at > now() - interval '15 minutes'))
	  ORDER BY verify_requested_at LIMIT 50`, projectList(projects))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Domain, error) {
		var d Domain
		return d, r.Scan(&d.ID, &d.ProjectID, &d.Host, &d.Token, &d.Method)
	})
}

// MarkVerified records a successful verification.
func (s *Store) MarkVerified(ctx context.Context, id int64, method string) error {
	_, err := s.pg.Exec(ctx, `UPDATE loadtest_domains SET verified_at = now(), method = $2, last_checked_at = now(), last_error = NULL WHERE id = $1`, id, method)
	return err
}

// MarkCheckFailed records a failed verification attempt.
func (s *Store) MarkCheckFailed(ctx context.Context, id int64, msg string) error {
	_, err := s.pg.Exec(ctx, `UPDATE loadtest_domains SET last_checked_at = now(), last_error = $2 WHERE id = $1`, id, msg)
	return err
}

// GetRun loads a run with its scenario.
func (s *Store) GetRun(ctx context.Context, id int64) (*Run, error) {
	var r Run
	var started *time.Time
	var vum *float64
	err := s.pg.QueryRow(ctx, `SELECT r.id, r.scenario_id, s.project_id, s.spec, r.state, r.started_at, r.requested_by,
	  (r.abort_requested_at IS NOT NULL OR r.state = 'aborted'), r.vu_minutes::float8
	  FROM loadtest_runs r JOIN loadtest_scenarios s ON s.id = r.scenario_id WHERE r.id = $1`, id).
		Scan(&r.ID, &r.ScenarioID, &r.ProjectID, &r.Spec, &r.State, &started, &r.RequestedBy, &r.Abort, &vum)
	if err != nil {
		return nil, err
	}
	if started != nil {
		r.StartedAt = *started
	}
	if vum != nil {
		r.VUMinutes = *vum
	}
	return &r, nil
}

// RunControl returns what a runner needs to keep going: the run state and whether an abort was requested.
func (s *Store) RunControl(ctx context.Context, id int64) (state string, abort bool, err error) {
	err = s.pg.QueryRow(ctx, `SELECT state, abort_requested_at IS NOT NULL FROM loadtest_runs WHERE id = $1`, id).Scan(&state, &abort)
	return
}

// RequestAbort sets abort_requested_at (idempotent).
func (s *Store) RequestAbort(ctx context.Context, id int64) error {
	_, err := s.pg.Exec(ctx, `UPDATE loadtest_runs SET abort_requested_at = coalesce(abort_requested_at, now()) WHERE id = $1`, id)
	return err
}

// RunningRuns lists running runs claimed by instance.
func (s *Store) RunningRuns(ctx context.Context, instance string) ([]int64, error) {
	rows, err := s.pg.Query(ctx, `SELECT id FROM loadtest_runs WHERE state = 'running' AND claimed_by = $1 ORDER BY id`, instance)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// StaleRuns lists running runs whose shards stopped reporting (no update for staleAfter), of any instance.
func (s *Store) StaleRuns(ctx context.Context, staleAfter time.Duration, projects []int64) ([]int64, error) {
	rows, err := s.pg.Query(ctx, `SELECT r.id FROM loadtest_runs r
	  WHERE r.state = 'running' AND r.started_at < now() - $1::interval
	    AND (cardinality($2::bigint[]) = 0 OR r.scenario_id IN (SELECT id FROM loadtest_scenarios WHERE project_id = ANY($2)))
	    AND coalesce((SELECT max(updated_at) FROM loadtest_run_shards sh WHERE sh.run_id = r.id), r.started_at) < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int(staleAfter.Seconds())), projectList(projects))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// Shard is one region runner's row.
type Shard struct {
	RunID       int64
	Region      string
	Share       float64
	State       string
	Live        json.RawMessage
	Stats       json.RawMessage
	Summary     json.RawMessage
	Error       string
	AbortReason string
	UpdatedAt   time.Time
}

// Terminal reports whether the shard finished.
func (s Shard) Terminal() bool {
	return s.State == "done" || s.State == "aborted" || s.State == "error"
}

// Shards returns a run's shards.
func (s *Store) Shards(ctx context.Context, run int64) ([]Shard, error) {
	rows, err := s.pg.Query(ctx, `SELECT run_id, region, share, state, live, stats, summary, coalesce(error, ''), coalesce(abort_reason, ''), updated_at
	  FROM loadtest_run_shards WHERE run_id = $1 ORDER BY region`, run)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Shard, error) {
		var sh Shard
		return sh, r.Scan(&sh.RunID, &sh.Region, &sh.Share, &sh.State, &sh.Live, &sh.Stats, &sh.Summary, &sh.Error, &sh.AbortReason, &sh.UpdatedAt)
	})
}

// ShardStart marks a shard running.
func (s *Store) ShardStart(ctx context.Context, run int64, region string) error {
	_, err := s.pg.Exec(ctx, `INSERT INTO loadtest_run_shards (run_id, region, state, started_at) VALUES ($1, $2, 'running', now())
	  ON CONFLICT (run_id, region) DO UPDATE SET state = 'running', started_at = now(), updated_at = now()`, run, region)
	return err
}

// ShardUpdate stores a shard's live snapshot and cumulative stats.
func (s *Store) ShardUpdate(ctx context.Context, run int64, region string, live, stats any) error {
	lb, _ := json.Marshal(live)
	sb, _ := json.Marshal(stats)
	_, err := s.pg.Exec(ctx, `UPDATE loadtest_run_shards SET live = $3, stats = $4, updated_at = now()
	  WHERE run_id = $1 AND region = $2 AND state IN ('pending', 'running')`, run, region, lb, sb)
	return err
}

// ShardFinish stores a shard's final state.
func (s *Store) ShardFinish(ctx context.Context, run int64, region, state string, stats, summary any, errMsg, abortReason string) error {
	sb, _ := json.Marshal(stats)
	mb, _ := json.Marshal(summary)
	_, err := s.pg.Exec(ctx, `INSERT INTO loadtest_run_shards (run_id, region, state, stats, summary, error, abort_reason, finished_at)
	  VALUES ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), now())
	  ON CONFLICT (run_id, region) DO UPDATE SET state = $3, stats = coalesce($4, loadtest_run_shards.stats), summary = $5,
	    error = nullif($6, ''), abort_reason = nullif($7, ''), finished_at = now(), updated_at = now()`,
		run, region, state, nullJSON(sb), nullJSON(mb), errMsg, abortReason)
	return err
}

// FailShard marks a non-terminal shard as error (runner lost / exited).
func (s *Store) FailShard(ctx context.Context, run int64, region, msg string) error {
	_, err := s.pg.Exec(ctx, `UPDATE loadtest_run_shards SET state = 'error', error = $3, finished_at = now(), updated_at = now()
	  WHERE run_id = $1 AND region = $2 AND state IN ('pending', 'running')`, run, region, msg)
	return err
}

func nullJSON(b []byte) []byte {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return b
}

// UpdateLive writes loadtest_runs.live and the running summary (what the api's run model reads).
func (s *Store) UpdateLive(ctx context.Context, run int64, live, summary any) error {
	lb, _ := json.Marshal(live)
	sb, _ := json.Marshal(summary)
	_, err := s.pg.Exec(ctx, `UPDATE loadtest_runs SET live = $2, summary = $3 WHERE id = $1 AND state = 'running'`, run, lb, sb)
	return err
}

// FinishRun stores the final state and settles usage (reserved → actual VU-minutes).
func (s *Store) FinishRun(ctx context.Context, run int64, state string, summary any, passed *bool, errMsg string, vuMinutes float64) error {
	sb, _ := json.Marshal(summary)
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var project int64
	var reserved *float64
	var started *time.Time
	if err := tx.QueryRow(ctx, `SELECT s.project_id, r.vu_minutes::float8, r.started_at FROM loadtest_runs r
	  JOIN loadtest_scenarios s ON s.id = r.scenario_id WHERE r.id = $1 FOR UPDATE OF r`, run).Scan(&project, &reserved, &started); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE loadtest_runs SET state = $2, summary = $3, thresholds_passed = $4, error = nullif($5, ''),
	  finished_at = now(), vu_minutes = $6,
	  live = CASE WHEN live IS NULL THEN NULL ELSE live || '{"progress": 1}'::jsonb END WHERE id = $1`,
		run, state, sb, passed, errMsg, vuMinutes); err != nil {
		return err
	}
	delta := vuMinutes
	if reserved != nil {
		delta -= *reserved
	}
	if delta != 0 && started != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO loadtest_usage (project_id, month, vu_minutes)
		  VALUES ($1, date_trunc('month', $3::timestamptz AT TIME ZONE 'UTC')::date, greatest($2, 0))
		  ON CONFLICT (project_id, month) DO UPDATE SET vu_minutes = greatest(loadtest_usage.vu_minutes + $2, 0)`,
			project, delta, *started); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ErrorRun moves a running run to 'error' (orphan recovery) and asks its runners to stop.
func (s *Store) ErrorRun(ctx context.Context, run int64, msg string) error {
	_, err := s.pg.Exec(ctx, `UPDATE loadtest_runs SET state = 'error', error = $2, finished_at = now(),
	  abort_requested_at = coalesce(abort_requested_at, now()),
	  summary = coalesce(summary, '{}'::jsonb) || jsonb_build_object('error', $2::text) WHERE id = $1 AND state = 'running'`, run, msg)
	if err != nil {
		return err
	}
	_, err = s.pg.Exec(ctx, `UPDATE loadtest_run_shards SET state = 'error', error = $2, finished_at = now(), updated_at = now()
	  WHERE run_id = $1 AND state IN ('pending', 'running')`, run, msg)
	return err
}

func projectList(p []int64) []int64 {
	if p == nil {
		return []int64{}
	}
	return p
}
