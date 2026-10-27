package engine

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/burn"
	"github.com/tasktrooper/telemetry/services/alerting/internal/kpi"
	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
	"github.com/tasktrooper/telemetry/services/alerting/internal/rca"
	"github.com/tasktrooper/telemetry/services/alerting/internal/statemachine"
	"github.com/tasktrooper/telemetry/services/alerting/internal/store"
)

// Default burn-rate windows when a rule leaves them empty (fast burn).
const (
	defaultLong  = time.Hour
	defaultShort = 5 * time.Minute
	defaultBelow = 15 * time.Minute
)

func (e *Engine) evalKpis(ctx context.Context, now time.Time) {
	for _, k := range e.kpis {
		if !kpi.Supported(k) {
			e.warnOnce(fmt.Sprintf("k%d", k.ID), "KPI target not computable from rollups; skipped", "kpi", k.ID, "target", k.Target.Kind)
			continue
		}
		end, err := e.writeSLI(ctx, k, now)
		if err != nil {
			e.log.Error("sli_1m write failed", "kpi", k.ID, "err", err)
			continue
		}
		for _, r := range k.Rules {
			if err := e.evalRule(ctx, k, r, end, now); err != nil {
				e.log.Error("kpi rule evaluation failed", "kpi", k.ID, "rule", r.ID, "err", err)
				continue
			}
			e.remindRule(ctx, k, r, now)
		}
	}
}

// writeSLI appends settled minutes to sli_1m and returns the exclusive end of the written range.
func (e *Engine) writeSLI(ctx context.Context, k *model.Kpi, now time.Time) (time.Time, error) {
	end := now.Truncate(time.Minute).Add(-e.cfg.SLISettle + time.Minute) // minutes < end are settled
	mark, ok := e.sliMark[k.ID]
	if !ok {
		wm, err := kpi.Watermark(ctx, e.ch, k)
		if err != nil {
			return end, err
		}
		mark = wm
	}
	start := end.Add(-e.cfg.SLIBackfill)
	if !mark.IsZero() && mark.Add(time.Minute).After(start) {
		start = mark.Add(time.Minute)
	}
	if !start.Before(end) {
		e.sliMark[k.ID] = mark
		return end, nil
	}
	aggs, err := kpi.Aggregates(ctx, e.ch, k, start, end)
	if err != nil {
		return end, err
	}
	ms := kpi.ToMinutes(k, aggs)
	if err := kpi.Write(ctx, e.ch, k, ms); err != nil {
		return end, err
	}
	e.reg.Add("tt_alert_sli_minutes_written_total", float64(len(ms)))
	e.sliMark[k.ID] = end.Add(-time.Minute)
	return end, nil
}

// RuleTitle is the deterministic incident title of a KPI rule (also its open-incident key).
func RuleTitle(k *model.Kpi, r model.KpiRule) string {
	if r.Kind == "below_target" {
		w := r.LongWindow
		if w <= 0 {
			w = defaultBelow
		}
		return fmt.Sprintf("%s: below %.4g%% over %s", k.Name, belowThreshold(r.Threshold)*100, fmtDur(w))
	}
	l, s := windows(r)
	return fmt.Sprintf("%s: error budget burning > %.4g× (%s/%s)", k.Name, r.Threshold, fmtDur(l), fmtDur(s))
}

func fmtDur(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

func windows(r model.KpiRule) (time.Duration, time.Duration) {
	l, s := r.LongWindow, r.ShortWindow
	if l <= 0 {
		l = defaultLong
	}
	if s <= 0 {
		s = l / 12 // SRE workbook ratio: 1h/5m, 6h/30m
	}
	return l, s
}

// belowThreshold accepts 0.97 or 97.
func belowThreshold(th float64) float64 {
	if th > 1 {
		return th / 100
	}
	return th
}

// RuleCheck evaluates one KPI rule on window counts (pure; used by tests).
func RuleCheck(r model.KpiRule, objective float64, long, short burn.Counts) (value float64, breached, noData bool) {
	if r.Kind == "below_target" {
		ratio := long.Ratio()
		if math.IsNaN(ratio) {
			return 0, false, true
		}
		return ratio, ratio < belowThreshold(r.Threshold), false
	}
	if long.Total <= 0 {
		return 0, false, true
	}
	res := burn.Check(burn.Window{Long: long, Short: short, Factor: r.Threshold}, objective)
	return res.LongRate, res.Breached, false
}

func (e *Engine) evalRule(ctx context.Context, k *model.Kpi, r model.KpiRule, end, now time.Time) error {
	title := RuleTitle(k, r)
	rs := e.rules[r.ID]
	if rs == nil {
		// (re)acquired: state from kpi_alert_rules, open incident from incidents.kpi_rule_id
		id, err := e.st.OpenKpiIncident(ctx, r.ID, k.ID, title)
		if err != nil {
			return err
		}
		snap := statemachine.Snapshot{State: r.State, Since: r.StateSince}
		if snap.State == "" {
			snap.State = model.StateOK
		}
		if snap.Since.IsZero() {
			snap.Since = now
		}
		if id != 0 {
			snap.State = model.StateFiring
		} else if snap.State == model.StateFiring {
			snap = statemachine.Snapshot{State: model.StateOK, Since: now} // incident closed elsewhere
		}
		rs = &ruleState{snap: snap, incidentID: id}
		e.rules[r.ID] = rs
	}

	var long, short burn.Counts
	var err error
	if r.Kind == "below_target" {
		w := r.LongWindow
		if w <= 0 {
			w = defaultBelow
		}
		long, err = kpi.Window(ctx, e.ch, k, end, w)
	} else {
		lw, sw := windows(r)
		if long, err = kpi.Window(ctx, e.ch, k, end, lw); err == nil {
			short, err = kpi.Window(ctx, e.ch, k, end, sw)
		}
	}
	if err != nil {
		return err
	}
	value, breached, noData := RuleCheck(r, k.Objective, long, short)
	next, tr := statemachine.Step(rs.snap, statemachine.Obs{Breached: breached, NoData: noData}, statemachine.ConfigFor(0), now)

	service := k.Target.Svc()
	endpoint := ""
	if k.Target.Kind == "endpoint" {
		endpoint = k.Target.Ref
	}
	kid, rid := k.ID, r.ID
	sup := e.kpiSuppressed(k, service)
	switch tr {
	case statemachine.Fire:
		var root *rca.Result
		if k.Target.Kind == "endpoint" {
			method, route := model.SplitEndpoint(k.Target.Ref)
			unit := rca.UnitRatio
			if k.SLI == "latency" {
				unit = rca.UnitMs
			}
			root = e.analyze(ctx, k.OrgID, k.ProjectID, service, route, method, k.Target.Env, unit, now)
		}
		inc := store.Incident{
			ProjectID: int64(k.ProjectID), KpiID: &kid, KpiRuleID: &rid, Title: title, Severity: r.Severity, Service: service, Endpoint: endpoint,
			Value: value, Channels: r.Channels, Suppressed: sup,
			Event: map[string]any{"ruleId": r.ID, "value": value, "goodRatioLong": long.Ratio(), "goodRatioShort": short.Ratio(), "objective": k.Objective},
		}
		if root != nil {
			inc.RootCause = root
		}
		deploy := e.deployContext(ctx, k.ProjectID, service, now)
		id, _, err := e.st.Open(ctx, inc, nil, func(id int64, started time.Time) map[string]any {
			p := map[string]any{
				"incidentId": id, "projectId": k.ProjectID, "kpiId": k.ID, "ruleId": r.ID, "kind": "kpi_" + r.Kind, "title": title,
				"severity": r.Severity, "state": "firing", "service": service, "endpoint": endpoint, "value": value,
				"threshold": r.Threshold, "objective": k.Objective, "startedAt": started.UnixMilli(), "link": e.link(id),
			}
			addRCA(p, root)
			if deploy != nil {
				p["deploy"] = deploy
			}
			return p
		})
		if err != nil {
			return err
		}
		rs.incidentID = id
		e.log.Info("kpi incident opened", "incident", id, "kpi", k.ID, "rule", r.ID, "burn", value)
	case statemachine.Resolve:
		if rs.incidentID != 0 {
			_, _, err := e.st.Resolve(ctx, 0, rs.incidentID, nil, r.Channels, map[string]any{"value": value}, sup, func(res store.Resolved) map[string]any {
				p := map[string]any{
					"incidentId": res.ID, "projectId": k.ProjectID, "kpiId": k.ID, "ruleId": r.ID, "kind": "kpi_" + r.Kind,
					"title": title, "severity": r.Severity, "state": "resolved", "service": service, "endpoint": endpoint,
					"value": value, "peak": res.Peak, "startedAt": res.StartedAt.UnixMilli(), "resolvedAt": res.ResolvedAt.UnixMilli(),
					"link": e.link(res.ID),
				}
				addCause(p, res.Summary, res.Confidence)
				return p
			})
			if err != nil {
				return err
			}
		}
		e.log.Info("kpi incident resolved", "incident", rs.incidentID, "kpi", k.ID, "rule", r.ID)
		rs.incidentID = 0
	default:
		if next.State == model.StateFiring && rs.incidentID != 0 && !noData {
			if err := e.st.PeakIncident(ctx, rs.incidentID, value, r.Kind == "below_target"); err != nil {
				return err
			}
		}
	}
	if tr != statemachine.None {
		e.reg.Add("tt_alert_transitions_total", 1, "to", tr.String())
	}
	var vp *float64
	if !noData && !math.IsNaN(value) && !math.IsInf(value, 0) {
		vp = &value
	}
	if err := e.st.SaveRuleState(ctx, r.ID, next.State, next.Since, vp, now); err != nil {
		return err
	}
	rs.snap = next
	rs.lastValue = vp
	return nil
}

func (e *Engine) kpiSuppressed(k *model.Kpi, service string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sup.Reason(int64(k.ProjectID), 0, service)
}

// remindRule re-sends a still-firing, unacknowledged KPI incident every condition.renotifyMin (mirrors
// remindMonitors: reminder n once per incident, suppressed while muted / in maintenance).
func (e *Engine) remindRule(ctx context.Context, k *model.Kpi, r model.KpiRule, now time.Time) {
	rs := e.rules[r.ID]
	if r.Renotify <= 0 || rs == nil || rs.snap.State != model.StateFiring || rs.incidentID == 0 {
		return
	}
	title := RuleTitle(k, r)
	service := k.Target.Svc()
	endpoint := ""
	if k.Target.Kind == "endpoint" {
		endpoint = k.Target.Ref
	}
	due, sent, err := e.st.Remind(ctx, store.IncidentSel{KpiRuleID: r.ID}, r.Renotify, now, r.Channels, e.kpiSuppressed(k, service), func(d store.ReminderDue) map[string]any {
		p := map[string]any{
			"incidentId": d.IncidentID, "projectId": k.ProjectID, "kpiId": k.ID, "ruleId": r.ID, "kind": "kpi_" + r.Kind, "title": title,
			"severity": r.Severity, "state": "firing", "reminder": d.N, "service": service, "endpoint": endpoint,
			"threshold": r.Threshold, "objective": k.Objective, "peak": d.Peak, "startedAt": d.StartedAt.UnixMilli(), "link": e.link(d.IncidentID),
		}
		if rs.lastValue != nil {
			p["value"] = *rs.lastValue
		}
		addCause(p, d.Summary, d.Confidence)
		return p
	})
	if err != nil {
		e.log.Error("kpi reminder failed", "kpi", k.ID, "rule", r.ID, "err", err)
		return
	}
	if sent {
		e.reg.Add("tt_alert_reminders_total", 1)
		e.log.Info("kpi reminder enqueued", "incident", due.IncidentID, "kpi", k.ID, "rule", r.ID, "n", due.N)
	}
}
