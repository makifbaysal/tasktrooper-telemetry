package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

// scenarioSpec is the subset of the UI ScenarioSpec (apps/web/src/lib/loadtest.ts) the list view derives from.
type scenarioSpec struct {
	Target    string   `json:"target"`
	Services  []string `json:"services"`
	Regions   []string `json:"regions"`
	Steps     []string `json:"steps"`
	Mode      string   `json:"mode"`
	ThinkTime float64  `json:"thinkTime"`
	Stages    []struct {
		Duration float64 `json:"duration"`
		Target   float64 `json:"target"`
	} `json:"stages"`
	Levels []struct {
		Ramp   float64 `json:"ramp"`
		Hold   float64 `json:"hold"`
		Target float64 `json:"target"`
	} `json:"levels"`
}

func summarize(id int64, name, source, state string, spec []byte, lastRun *int64) model.LoadTestScenario {
	s := model.LoadTestScenario{ID: strconv.FormatInt(id, 10), Name: name, Source: source, State: state, Spec: json.RawMessage(spec), Regions: []string{}}
	var sp scenarioSpec
	_ = json.Unmarshal(spec, &sp)
	s.Target, s.Steps, s.Services = sp.Target, len(sp.Steps), sp.Services
	if sp.Regions != nil {
		s.Regions = sp.Regions
	}
	peak := 0.0
	if len(sp.Levels) > 0 {
		for _, l := range sp.Levels {
			s.Duration += l.Ramp + l.Hold
			peak = math.Max(peak, l.Target)
		}
	} else {
		for _, st := range sp.Stages {
			s.Duration += st.Duration
			peak = math.Max(peak, st.Target)
		}
	}
	s.PeakVus = peak
	if sp.Mode == "rps" { // open model: VUs ≈ arrival rate × (latency + think time), as in saveScenario()
		s.PeakVus = math.Max(1, math.Ceil(peak*(0.2+sp.ThinkTime)))
	}
	if lastRun != nil {
		s.LastRun = strconv.FormatInt(*lastRun, 10)
	}
	return s
}

const scenarioSelect = `SELECT s.id, s.name, s.source, coalesce(s.state, 'ready'), s.spec, (SELECT max(r.id) FROM loadtest_runs r WHERE r.scenario_id = s.id) FROM loadtest_scenarios s`

// ListScenarios returns load test scenarios.
func (d *DB) ListScenarios(ctx context.Context) ([]model.LoadTestScenario, error) {
	rows, err := d.pg.Query(ctx, scenarioSelect+` WHERE {{tenant:s}} ORDER BY s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.LoadTestScenario{}
	for rows.Next() {
		var id int64
		var name, source, state string
		var spec []byte
		var last *int64
		if err := rows.Scan(&id, &name, &source, &state, &spec, &last); err != nil {
			return nil, err
		}
		out = append(out, summarize(id, name, source, state, spec, last))
	}
	return out, rows.Err()
}

// GetScenario returns one scenario.
func (d *DB) GetScenario(ctx context.Context, id string) (*model.LoadTestScenario, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	var name, source, state string
	var spec []byte
	var last *int64
	err = d.pg.QueryRow(ctx, scenarioSelect+` WHERE {{tenant:s}} AND s.id = $1`, n).Scan(&n, &name, &source, &state, &spec, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s := summarize(n, name, source, state, spec, last)
	return &s, nil
}

func validateScenario(in *model.ScenarioInput) error {
	if strings.TrimSpace(in.Name) == "" {
		return Invalid("name is required")
	}
	if in.Source == "" {
		in.Source = "manual"
	}
	if in.Source != "manual" && in.Source != "generated" {
		return Invalid("source must be manual|generated")
	}
	if len(in.Spec) == 0 || !json.Valid(in.Spec) || in.Spec[0] != '{' {
		return Invalid("spec must be a JSON object")
	}
	if in.State != "" && in.State != "draft" && in.State != "ready" {
		return Invalid("state must be draft|ready")
	}
	return nil
}

// SetScenarioState marks a (generated) scenario draft or ready; drafts cannot be run.
func (d *DB) SetScenarioState(ctx context.Context, id, state string) (*model.LoadTestScenario, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	if state != "draft" && state != "ready" {
		return nil, Invalid("state must be draft|ready")
	}
	tag, err := d.pg.Exec(ctx, `UPDATE loadtest_scenarios SET state = $2 WHERE {{tenant}} AND id = $1`, n, state)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return d.GetScenario(ctx, id)
}

// CreateScenario stores a scenario.
func (d *DB) CreateScenario(ctx context.Context, in model.ScenarioInput) (*model.LoadTestScenario, error) {
	if err := validateScenario(&in); err != nil {
		return nil, err
	}
	var id int64
	if in.State == "" {
		in.State = "ready"
	}
	if err := d.pg.QueryRow(ctx, `INSERT INTO loadtest_scenarios (project_id, name, spec, source, state) VALUES ({{project}}, $1, $2, $3, $4) RETURNING id`,
		in.Name, []byte(in.Spec), in.Source, in.State).Scan(&id); err != nil {
		return nil, err
	}
	return d.GetScenario(ctx, strconv.FormatInt(id, 10))
}

// UpdateScenario replaces a scenario.
func (d *DB) UpdateScenario(ctx context.Context, id string, in model.ScenarioInput) (*model.LoadTestScenario, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	if err := validateScenario(&in); err != nil {
		return nil, err
	}
	tag, err := d.pg.Exec(ctx, `UPDATE loadtest_scenarios SET name = $2, spec = $3, source = $4, state = coalesce(nullif($5, ''), state)
	  WHERE {{tenant}} AND id = $1`, n, in.Name, []byte(in.Spec), in.Source, in.State)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return d.GetScenario(ctx, id)
}

// DeleteScenario removes a scenario and its runs.
func (d *DB) DeleteScenario(ctx context.Context, id string) error {
	n, err := parseID(id)
	if err != nil {
		return err
	}
	tag, err := d.pg.Exec(ctx, `DELETE FROM loadtest_scenarios WHERE {{tenant}} AND id = $1`, n)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type runSummary struct {
	Steps       json.RawMessage   `json:"steps"`
	StatusCodes json.RawMessage   `json:"statusCodes"`
	Progress    float64           `json:"progress"`
	Rps         float64           `json:"rps"`
	P95         float64           `json:"p95"`
	ErrorRate   float64           `json:"errorRate"`
	Vus         float64           `json:"vus"`
	Thresholds  []model.Threshold `json:"thresholds"`
}

const runSelect = `SELECT r.id, r.scenario_id, r.state, r.started_at, r.finished_at, r.summary, r.created_at, r.live,
  coalesce(r.error, ''), coalesce(r.region, ''), r.abort_requested_at, r.thresholds_passed
  FROM loadtest_runs r JOIN loadtest_scenarios s ON s.id = r.scenario_id`

func scanRun(row pgx.Row) (model.LoadTestRun, error) {
	var r model.LoadTestRun
	var id, sid int64
	var started, finished, abort *time.Time
	var created time.Time
	var summary, live []byte
	if err := row.Scan(&id, &sid, &r.State, &started, &finished, &summary, &created, &live, &r.Error, &r.Region, &abort, &r.ThresholdsPassed); err != nil {
		return r, err
	}
	r.ID, r.ScenarioID, r.CreatedAt = strconv.FormatInt(id, 10), strconv.FormatInt(sid, 10), ms(created)
	r.StartedAt = r.CreatedAt // queued: not started yet
	if started != nil {
		r.StartedAt = ms(*started)
	}
	if finished != nil {
		r.FinishedAt = ms(*finished)
	}
	if abort != nil {
		r.AbortRequestedAt = ms(*abort)
	}
	var s runSummary
	_ = json.Unmarshal(summary, &s)
	r.Progress, r.Rps, r.P95, r.ErrorRate, r.Vus, r.Thresholds = s.Progress, s.Rps, s.P95, s.ErrorRate, s.Vus, s.Thresholds
	r.Steps, r.StatusCodes = s.Steps, s.StatusCodes
	if len(live) > 0 && string(live) != "null" {
		var l model.RunLive
		if json.Unmarshal(live, &l) == nil {
			r.Live = &l
			if r.State == "running" || summary == nil || (s.Rps == 0 && s.Vus == 0) { // the summary is written at the end
				r.Progress, r.Rps, r.P95, r.ErrorRate, r.Vus = l.Progress, l.Rps, l.P95, l.ErrorRate, l.Vus
			}
		}
	}
	if r.Thresholds == nil {
		r.Thresholds = []model.Threshold{}
	}
	return r, nil
}

// ListRuns returns runs, newest first (optionally of one scenario).
func (d *DB) ListRuns(ctx context.Context, scenarioID string) ([]model.LoadTestRun, error) {
	where, args := "", []any{}
	if scenarioID != "" {
		n, err := parseID(scenarioID)
		if err != nil {
			return nil, err
		}
		where, args = " AND s.id = $1", []any{n}
	}
	rows, err := d.pg.Query(ctx, runSelect+` WHERE {{tenant:s}}`+where+` ORDER BY r.id DESC LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.LoadTestRun{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRun returns one run.
func (d *DB) GetRun(ctx context.Context, id string) (*model.LoadTestRun, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	r, err := scanRun(d.pg.QueryRow(ctx, runSelect+` WHERE {{tenant:s}} AND r.id = $1`, n))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &r, err
}

// CreateRun queues a run of a ready scenario (loadtest-ctl picks queued runs up; started_at is set when it starts).
func (d *DB) CreateRun(ctx context.Context, scenarioID string, requestedBy int64) (*model.LoadTestRun, error) {
	n, err := parseID(scenarioID)
	if err != nil {
		return nil, err
	}
	var id int64
	err = d.pg.QueryRow(ctx, `INSERT INTO loadtest_runs (scenario_id, state, summary, requested_by)
	  SELECT id, 'queued', '{"progress": 0}'::jsonb, $2 FROM loadtest_scenarios WHERE {{tenant}} AND id = $1 AND state <> 'draft' RETURNING id`,
		n, nullID(requestedBy)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		sc, gerr := d.GetScenario(ctx, scenarioID)
		if gerr != nil {
			return nil, gerr
		}
		if sc.State == "draft" {
			return nil, Conflict("scenario is a draft: review it and mark it ready (PATCH state=ready) before running")
		}
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return d.GetRun(ctx, strconv.FormatInt(id, 10))
}

// StopRun aborts a run: a queued run is aborted right away, a running one gets abort_requested_at and loadtest-ctl
// stops its runners. Finished runs → 409.
func (d *DB) StopRun(ctx context.Context, id string) (*model.LoadTestRun, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	tag, err := d.pg.Exec(ctx, `UPDATE loadtest_runs SET abort_requested_at = coalesce(abort_requested_at, now()),
	    finished_at = CASE WHEN state = 'queued' THEN now() ELSE finished_at END,
	    state = CASE WHEN state = 'queued' THEN 'aborted' ELSE state END
	  WHERE id = $1 AND state IN ('queued', 'running') AND scenario_id IN (SELECT id FROM loadtest_scenarios WHERE {{tenant}})`, n)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := d.GetRun(ctx, id); err != nil {
			return nil, err
		}
		return nil, Conflict("run is not queued or running")
	}
	return d.GetRun(ctx, id)
}
