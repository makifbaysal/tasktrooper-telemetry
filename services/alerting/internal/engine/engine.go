// Package engine runs the evaluation loop: leader election, monitor/KPI cache, evaluation, state machine,
// incidents, RCA and the notification outbox.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/eval"
	"github.com/tasktrooper/telemetry/services/alerting/internal/kpi"
	"github.com/tasktrooper/telemetry/services/alerting/internal/leader"
	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
	"github.com/tasktrooper/telemetry/services/alerting/internal/obs"
	"github.com/tasktrooper/telemetry/services/alerting/internal/rca"
	"github.com/tasktrooper/telemetry/services/alerting/internal/statemachine"
	"github.com/tasktrooper/telemetry/services/alerting/internal/store"
)

// Config tunes the loop.
type Config struct {
	Interval    time.Duration // evaluation period (TT_ALERT_INTERVAL, 30s)
	Refresh     time.Duration // full monitor reload even without changes (5m)
	EvalTimeout time.Duration // per monitor (10s)
	RCATimeout  time.Duration // per incident (15s)
	Workers     int           // concurrent evaluations (8)
	SLIBackfill time.Duration // how far back sli_1m is filled for a new KPI (6h)
	SLISettle   time.Duration // minutes younger than this are not written to sli_1m yet (2m)
	PublicURL   string        // dashboard base URL for notification links
	ProjectIDs  []int64       // optional: only these projects (dedicated deployments, tests)
}

func (c *Config) defaults() {
	if c.Interval <= 0 {
		c.Interval = 30 * time.Second
	}
	if c.Refresh <= 0 {
		c.Refresh = 5 * time.Minute
	}
	if c.EvalTimeout <= 0 {
		c.EvalTimeout = 10 * time.Second
	}
	if c.RCATimeout <= 0 {
		c.RCATimeout = 15 * time.Second
	}
	if c.Workers <= 0 {
		c.Workers = 8
	}
	if c.SLIBackfill <= 0 {
		c.SLIBackfill = 6 * time.Hour
	}
	if c.SLISettle <= 0 {
		c.SLISettle = 2 * time.Minute
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
}

// Engine is the alert engine.
type Engine struct {
	cfg     Config
	log     *slog.Logger
	st      *store.Store
	ch      kpi.CH
	ev      *eval.Evaluator
	elector *leader.Elector
	reg     *obs.Registry
	Now     func() time.Time

	mu       sync.Mutex
	held     []int
	monitors []*model.Monitor
	kpis     []*model.Kpi
	fp       string
	loadedAt time.Time
	clear    map[int64]time.Time // monitor id → recovery hysteresis start (in memory)
	rules    map[int64]*ruleState
	sliMark  map[int64]time.Time // kpi id → last minute written to sli_1m
	warned   map[string]bool
	locks    map[int64]*sync.Mutex
	sup      store.Suppression
	// streamOthers: other shards' stream rules (stream leader with N > 1; hits are handed off)
	streamOthers []*model.Monitor
}

type ruleState struct {
	snap       statemachine.Snapshot
	incidentID int64
	lastValue  *float64
}

// New builds an engine.
func New(cfg Config, log *slog.Logger, st *store.Store, ch kpi.CH, el *leader.Elector, reg *obs.Registry) *Engine {
	cfg.defaults()
	reg.Describe("tt_alert_leader", "gauge", "1 when this instance holds at least one shard")
	reg.Describe("tt_alert_shards_held", "gauge", "shards held by this instance")
	reg.Describe("tt_alert_monitors", "gauge", "active monitors evaluated by this instance")
	reg.Describe("tt_alert_kpis", "gauge", "KPIs evaluated by this instance")
	reg.Describe("tt_alert_evaluations_total", "counter", "monitor evaluations by kind and outcome")
	reg.Describe("tt_alert_transitions_total", "counter", "state machine transitions")
	reg.Describe("tt_alert_cycle_seconds", "gauge", "duration of the last evaluation cycle")
	reg.Describe("tt_alert_last_cycle_timestamp_seconds", "gauge", "unix time of the last completed cycle")
	reg.Describe("tt_alert_rca_total", "counter", "root cause analyses by outcome")
	reg.Describe("tt_alert_sli_minutes_written_total", "counter", "sli_1m rows written")
	reg.Describe("tt_alert_reminders_total", "counter", "reminder notifications enqueued")
	reg.Describe("tt_alert_stream_records_total", "counter", "stream records processed by topic")
	reg.Describe("tt_alert_stream_matches_total", "counter", "stream records matching a stream rule")
	reg.Describe("tt_alert_stream_handoffs_total", "counter", "stream hits handed to / claimed from other shards by direction")
	return &Engine{
		cfg: cfg, log: log, st: st, ch: ch, ev: newEvaluator(ch, st), elector: el, reg: reg, Now: time.Now,
		clear: map[int64]time.Time{}, rules: map[int64]*ruleState{}, sliMark: map[int64]time.Time{}, warned: map[string]bool{},
		locks: map[int64]*sync.Mutex{},
	}
}

// Run ticks until ctx ends.
func (e *Engine) Run(ctx context.Context) error {
	t := time.NewTicker(e.cfg.Interval)
	defer t.Stop()
	defer e.elector.Release()
	for {
		if err := e.Tick(ctx); err != nil && ctx.Err() == nil {
			e.log.Error("cycle failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick runs one cycle: elect, refresh cache, evaluate monitors and KPIs.
func (e *Engine) Tick(ctx context.Context) error {
	start := time.Now()
	held, err := e.elector.Tick(ctx)
	if err != nil {
		e.reg.Set("tt_alert_leader", 0)
		return fmt.Errorf("leader election: %w", err)
	}
	e.reg.Set("tt_alert_shards_held", float64(len(held)))
	if len(held) == 0 {
		e.reg.Set("tt_alert_leader", 0)
		if len(e.held) > 0 {
			e.log.Info("lost leadership; standing by")
		}
		e.reset(nil)
		return nil
	}
	e.reg.Set("tt_alert_leader", 1)
	if !slices.Equal(held, e.held) {
		e.log.Info("shards acquired", "held", held, "of", e.elector.Shards)
		e.reset(held)
	}
	if err := e.refresh(ctx); err != nil {
		return fmt.Errorf("load monitors: %w", err)
	}
	now := e.Now()
	sup, err := e.st.LoadSuppression(ctx, now, e.cfg.ProjectIDs)
	if err != nil {
		return fmt.Errorf("load mutes / maintenance windows: %w", err)
	}
	e.mu.Lock()
	e.sup = sup
	e.mu.Unlock()
	e.evalMonitors(ctx, now)
	e.remindMonitors(ctx, now)
	e.evalKpis(ctx, now)
	if e.StreamLeader() { // one instance (shard 0): Slack App acks for every shard
		if n, err := e.st.EnqueueAcks(ctx); err != nil {
			e.log.Error("enqueue ack notifications", "err", err)
		} else if n > 0 {
			e.log.Info("ack notifications enqueued", "count", n)
		}
	}
	e.reg.Set("tt_alert_cycle_seconds", time.Since(start).Seconds())
	e.reg.Set("tt_alert_last_cycle_timestamp_seconds", float64(time.Now().Unix()))
	return nil
}

// reset drops every cache (leadership changed: another instance may have written state meanwhile).
func (e *Engine) reset(held []int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.held = held
	e.monitors, e.kpis, e.fp = nil, nil, ""
	e.clear = map[int64]time.Time{}
	e.rules = map[int64]*ruleState{}
	e.sliMark = map[int64]time.Time{}
	e.streamOthers = nil
}

func (e *Engine) shards() store.Shards { return store.Shards{N: e.elector.Shards, Held: e.held} }

func (e *Engine) inScope(projectID uint32) bool {
	return len(e.cfg.ProjectIDs) == 0 || slices.Contains(e.cfg.ProjectIDs, int64(projectID))
}

func (e *Engine) refresh(ctx context.Context) error {
	fp, err := e.st.Fingerprint(ctx)
	if err != nil {
		return err
	}
	if fp == e.fp && time.Since(e.loadedAt) < e.cfg.Refresh {
		return nil
	}
	ms, err := e.st.LoadMonitors(ctx, e.shards())
	if err != nil {
		return err
	}
	ks, err := e.st.LoadKpis(ctx, e.shards())
	if err != nil {
		return err
	}
	ms = slices.DeleteFunc(ms, func(m *model.Monitor) bool { return !e.inScope(m.ProjectID) })
	e.mu.Lock()
	e.monitors = ms
	e.mu.Unlock()
	e.kpis = slices.DeleteFunc(ks, func(k *model.Kpi) bool { return !e.inScope(k.ProjectID) })
	if err := e.loadStreamOthers(ctx); err != nil {
		return err
	}
	e.fp, e.loadedAt = fp, time.Now()
	e.reg.Set("tt_alert_monitors", float64(len(e.monitors)))
	e.reg.Set("tt_alert_kpis", float64(len(e.kpis)))
	e.log.Debug("monitors loaded", "monitors", len(e.monitors), "kpis", len(e.kpis))
	return nil
}

func (e *Engine) warnOnce(key string, msg string, args ...any) {
	e.mu.Lock()
	seen := e.warned[key]
	e.warned[key] = true
	e.mu.Unlock()
	if !seen {
		e.log.Warn(msg, args...)
	}
}

func (e *Engine) evalMonitors(ctx context.Context, now time.Time) {
	sem := make(chan struct{}, e.cfg.Workers)
	var wg sync.WaitGroup
	var composites []*model.Monitor
	for _, m := range e.snapshot() {
		if m.Kind == "composite" {
			composites = append(composites, m)
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(m *model.Monitor) {
			defer func() {
				if r := recover(); r != nil {
					e.log.Error("monitor evaluation panicked", "monitor", m.ID, "panic", r)
				}
				<-sem
				wg.Done()
			}()
			e.evalMonitor(ctx, m, now, false)
		}(m)
	}
	wg.Wait()
	// composites read the states written above (and other shards' states from Postgres)
	for _, m := range composites {
		e.evalComposite(ctx, m, now)
	}
}

// snapshot returns the current monitor list (the slice is replaced, never mutated, on refresh).
func (e *Engine) snapshot() []*model.Monitor {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.monitors
}

func (e *Engine) lock(id int64) func() {
	e.mu.Lock()
	l := e.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		e.locks[id] = l
	}
	e.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// evalMonitor evaluates m against ClickHouse and applies the result. escalateOnly (stream triggers) only
// applies breaches: recovery is decided by the periodic evaluation.
func (e *Engine) evalMonitor(ctx context.Context, m *model.Monitor, now time.Time, escalateOnly bool) {
	ectx, cancel := context.WithTimeout(ctx, e.cfg.EvalTimeout)
	res, err := e.ev.Eval(ectx, m, now)
	cancel()
	switch {
	case err == nil:
	case errors.Is(err, eval.ErrUnsupported) || strings.HasPrefix(m.Kind, "invalid:") || res.NoData:
		e.warnOnce(fmt.Sprintf("m%d", m.ID), "monitor cannot be evaluated; marking no_data", "monitor", m.ID, "kind", m.Kind, "err", err)
		res = eval.Result{NoData: true}
	default:
		e.reg.Add("tt_alert_evaluations_total", 1, "kind", m.Kind, "outcome", "error")
		e.log.Error("evaluation failed", "monitor", m.ID, "kind", m.Kind, "err", err)
		return
	}
	e.apply(ctx, m, res, now, escalateOnly)
}

// evalComposite: {op: and|or, monitorIds}. Value = children firing; and → all firing, or → any firing.
// No known child (deleted / other project) → no_data; for `and` a missing child also means no_data.
func (e *Engine) evalComposite(ctx context.Context, m *model.Monitor, now time.Time) {
	ids := make([]int64, 0, len(m.Query.MonitorIDs))
	for _, id := range m.Query.MonitorIDs {
		if int64(id) != m.ID {
			ids = append(ids, int64(id))
		}
	}
	states, err := e.st.MonitorStates(ctx, int64(m.ProjectID), ids)
	if err != nil {
		e.reg.Add("tt_alert_evaluations_total", 1, "kind", m.Kind, "outcome", "error")
		e.log.Error("composite children", "monitor", m.ID, "err", err)
		return
	}
	e.apply(ctx, m, CompositeResult(m.Query.Op, ids, states), now, false)
}

// CompositeResult combines child states (pure; used by tests).
func CompositeResult(op string, ids []int64, states map[int64]model.State) eval.Result {
	firing, known := 0, 0
	detail := map[string]any{}
	for _, id := range ids {
		s, ok := states[id]
		if !ok {
			continue
		}
		known++
		detail[fmt.Sprint(id)] = string(s)
		if s == model.StateFiring {
			firing++
		}
	}
	and := !strings.EqualFold(op, "or")
	if known == 0 || (and && known < len(ids)) {
		return eval.Result{NoData: true, Detail: detail}
	}
	breached := firing > 0
	if and {
		breached = firing == len(ids)
	}
	detail["op"], detail["firing"] = strings.ToLower(orDefault(op, "and")), firing
	return eval.Result{Value: float64(firing), Breached: breached, Detail: detail}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// apply runs the state machine on one evaluation result and persists it (shared by the periodic loop and the
// stream consumer; serialised per monitor).
func (e *Engine) apply(ctx context.Context, m *model.Monitor, res eval.Result, now time.Time, escalateOnly bool) {
	if escalateOnly && (!res.Breached || res.NoData) {
		return
	}
	unlock := e.lock(m.ID)
	defer unlock()
	outcome := "ok"
	if res.NoData {
		outcome = "no_data"
	} else if res.Breached {
		outcome = "breached"
	}
	e.reg.Add("tt_alert_evaluations_total", 1, "kind", m.Kind, "outcome", outcome)

	cond := eval.Condition(m)
	e.mu.Lock()
	snap := statemachine.Snapshot{State: m.State, Since: m.StateSince, ClearSince: e.clear[m.ID]}
	e.mu.Unlock()
	next, tr := statemachine.Step(snap, statemachine.Obs{Breached: res.Breached, NoData: res.NoData, HeldFor: res.HeldFor}, statemachine.ConfigFor(cond.For), now)
	if escalateOnly && tr == statemachine.None && next.State == snap.State {
		return // already pending/firing: the periodic loop keeps value/peak up to date
	}

	var value *float64
	if !res.NoData && !math.IsNaN(res.Value) && !math.IsInf(res.Value, 0) {
		v := res.Value
		value = &v
	}
	ms := &store.MonitorState{ID: m.ID, State: next.State, Since: next.Since, Value: value, At: now}
	var perr error
	switch tr {
	case statemachine.Pending:
		perr = e.pending(ctx, m, res, cond, ms)
	case statemachine.Fire:
		perr = e.fire(ctx, m, res, cond, ms, now)
	case statemachine.Resolve:
		perr = e.resolve(ctx, m, res, ms)
	case statemachine.Cleared, statemachine.NoData:
		if snap.State == model.StatePending {
			// condition went away before forMin: resolve the pending incident quietly
			_, _, perr = e.st.Resolve(ctx, m.ID, 0, ms, nil, map[string]any{"value": res.Value, "transition": tr.String()}, "", func(store.Resolved) map[string]any { return nil })
		} else {
			perr = e.st.SaveEval(ctx, m.ID, next.State, next.Since, value, now)
		}
	default:
		perr = e.st.SaveEval(ctx, m.ID, next.State, next.Since, value, now)
		if perr == nil && (next.State == model.StateFiring || next.State == model.StatePending) && value != nil {
			perr = e.st.Peak(ctx, m.ID, *value, model.LowerIsWorse(cond.Op))
		}
	}
	if perr != nil {
		e.log.Error("persist failed", "monitor", m.ID, "transition", tr.String(), "err", perr)
		return // state stays as it was; the transition is retried next cycle
	}
	if tr != statemachine.None {
		e.reg.Add("tt_alert_transitions_total", 1, "to", tr.String())
		e.log.Info("monitor transition", "monitor", m.ID, "name", m.Name, "transition", tr.String(), "value", res.Value, "stream", escalateOnly)
	}
	e.mu.Lock()
	m.State, m.StateSince, m.LastValue = next.State, next.Since, value
	e.clear[m.ID] = next.ClearSince
	e.mu.Unlock()
}

// suppressed returns why notifications for m are suppressed now ("muted", "maintenance:<id>") or "".
// Mutes come from the reloaded monitor row (the api bumps updated_at when muted_until changes) and from the
// per-tick suppression snapshot (covers direct database edits and mutes expiring between reloads).
func (e *Engine) suppressed(m *model.Monitor, now time.Time) string {
	e.mu.Lock()
	until := m.MutedUntil
	e.mu.Unlock()
	if until != nil && until.After(now) {
		return "muted"
	}
	service, _, _, _ := m.Scope()
	e.mu.Lock()
	sup := e.sup
	e.mu.Unlock()
	return sup.Reason(int64(m.ProjectID), m.ID, service)
}

func (e *Engine) remindMonitors(ctx context.Context, now time.Time) {
	for _, m := range e.snapshot() {
		every := m.Condition.Renotify
		if every <= 0 || m.State != model.StateFiring {
			continue
		}
		service, route, method, _ := m.Scope()
		endpoint := strings.TrimSpace(method + " " + route)
		cond := eval.Condition(m)
		due, sent, err := e.st.Remind(ctx, store.IncidentSel{MonitorID: m.ID}, every, now, m.Channels, e.suppressed(m, now), func(d store.ReminderDue) map[string]any {
			p := map[string]any{
				"incidentId": d.IncidentID, "projectId": m.ProjectID, "monitorId": m.ID, "kind": m.Kind, "title": m.Name,
				"severity": m.Severity, "state": "firing", "reminder": d.N, "service": service, "endpoint": endpoint,
				"op": cond.Op, "threshold": cond.Threshold, "peak": d.Peak, "startedAt": d.StartedAt.UnixMilli(), "link": e.link(d.IncidentID),
			}
			if m.LastValue != nil {
				p["value"] = *m.LastValue
			}
			addCause(p, d.Summary, d.Confidence)
			addMonitorRefs(p, m)
			return p
		})
		if err != nil {
			e.log.Error("reminder failed", "monitor", m.ID, "err", err)
			continue
		}
		if sent {
			e.reg.Add("tt_alert_reminders_total", 1)
			e.log.Info("reminder enqueued", "incident", due.IncidentID, "monitor", m.ID, "n", due.N)
		}
	}
}

// rcaUnit returns the RCA unit for a monitor, or "" when RCA does not apply.
func rcaUnit(m *model.Monitor) rca.Unit {
	switch m.Kind {
	case "latency", "apdex":
		return rca.UnitMs
	case "error_rate":
		return rca.UnitRatio
	case "metric_threshold":
		switch strings.ToLower(m.Query.Metric) {
		case "error_rate", "er", "errors", "":
			return rca.UnitRatio
		case "rpm", "throughput", "requests":
			return ""
		}
		return rca.UnitMs
	}
	return ""
}

func (e *Engine) analyze(ctx context.Context, org, project uint32, service, route, method, env string, unit rca.Unit, now time.Time) *rca.Result {
	if service == "" {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, e.cfg.RCATimeout)
	defer cancel()
	var out *rca.Result
	if unit != "" {
		src := eval.NewRCASource(e.ch, org, project, env, now)
		res, _, err := rca.Analyze(rctx, src, service, route, method, unit)
		if err != nil {
			e.reg.Add("tt_alert_rca_total", 1, "outcome", "error")
			e.log.Warn("root cause analysis failed", "service", service, "route", route, "err", err)
		} else {
			e.reg.Add("tt_alert_rca_total", 1, "outcome", "ok")
			c := rca.Compact(res)
			out = &c
		}
	}
	// Kubernetes / host candidates of the service (contracts §20); best effort
	pods, hosts, err := eval.InfraSignals(rctx, e.ch, org, project, service, now)
	if err != nil {
		e.log.Warn("infra root cause signals failed", "service", service, "err", err)
		return out
	}
	return rca.ApplyInfra(out, service, unit, rca.InfraCandidates(pods, hosts))
}

func (e *Engine) link(incidentID int64) string {
	if e.cfg.PublicURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/incidents?id=%d", e.cfg.PublicURL, incidentID)
}

func (e *Engine) fire(ctx context.Context, m *model.Monitor, res eval.Result, cond model.Condition, ms *store.MonitorState, now time.Time) error {
	service, route, method, env := m.Scope()
	var root *rca.Result
	if m.Kind == "llm" {
		root = llmCause(res) // the contributing model / service, not the call tree
	} else {
		root = e.analyze(ctx, m.OrgID, m.ProjectID, service, route, method, env, rcaUnit(m), now)
	}
	endpoint := strings.TrimSpace(method + " " + route)
	mid := m.ID
	inc := store.Incident{
		ProjectID: int64(m.ProjectID), MonitorID: &mid, Title: m.Name, Severity: m.Severity,
		Service: service, Endpoint: endpoint, Value: res.Value, Channels: m.Channels,
		Event:      map[string]any{"value": res.Value, "op": cond.Op, "threshold": cond.Threshold, "detail": res.Detail},
		Suppressed: e.suppressed(m, now),
	}
	if root != nil {
		inc.RootCause = root // stored in incidents.root_cause in the same transaction that enqueues the notification
	}
	deploy := e.deployContext(ctx, m.ProjectID, service, now)
	payload := func(id int64, started time.Time) map[string]any {
		p := map[string]any{
			"incidentId": id, "projectId": m.ProjectID, "monitorId": m.ID, "kind": m.Kind, "title": m.Name,
			"severity": m.Severity, "state": "firing", "service": service, "endpoint": endpoint,
			"value": res.Value, "op": cond.Op, "threshold": cond.Threshold, "startedAt": started.UnixMilli(), "link": e.link(id),
		}
		if m.Kind == "llm" && root != nil {
			addCause(p, root.Summary, nil) // the summary already carries the contributor's share
		} else {
			addRCA(p, root)
		}
		addMonitorRefs(p, m)
		if deploy != nil {
			p["deploy"] = deploy
		}
		return p
	}
	id, created, err := e.st.Open(ctx, inc, ms, payload)
	if err == nil && created {
		e.log.Info("incident opened", "incident", id, "monitor", m.ID, "channels", m.Channels, "suppressed", inc.Suppressed)
	}
	return err
}

// pending records a pending incident (no notification) when the condition starts to hold.
func (e *Engine) pending(ctx context.Context, m *model.Monitor, res eval.Result, cond model.Condition, ms *store.MonitorState) error {
	service, route, method, _ := m.Scope()
	mid := m.ID
	_, err := e.st.OpenPending(ctx, store.Incident{
		ProjectID: int64(m.ProjectID), MonitorID: &mid, Title: m.Name, Severity: m.Severity, Service: service,
		Endpoint: strings.TrimSpace(method + " " + route), Value: res.Value,
		Event: map[string]any{"value": res.Value, "op": cond.Op, "threshold": cond.Threshold, "forMin": cond.For.Minutes(), "detail": res.Detail},
	}, ms)
	return err
}

func (e *Engine) resolve(ctx context.Context, m *model.Monitor, res eval.Result, ms *store.MonitorState) error {
	service, route, method, _ := m.Scope()
	endpoint := strings.TrimSpace(method + " " + route)
	payload := func(r store.Resolved) map[string]any {
		p := map[string]any{
			"incidentId": r.ID, "projectId": m.ProjectID, "monitorId": m.ID, "kind": m.Kind, "title": m.Name,
			"severity": m.Severity, "state": "resolved", "service": service, "endpoint": endpoint, "value": res.Value,
			"peak": r.Peak, "startedAt": r.StartedAt.UnixMilli(), "resolvedAt": r.ResolvedAt.UnixMilli(),
			"link": e.link(r.ID),
		}
		addCause(p, r.Summary, r.Confidence)
		addMonitorRefs(p, m)
		return p
	}
	_, _, err := e.st.Resolve(ctx, m.ID, 0, ms, m.Channels, map[string]any{"value": res.Value}, e.suppressed(m, ms.At), payload)
	return err
}
