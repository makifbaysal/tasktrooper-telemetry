package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/tq"
)

// Session replay (ADR-021, contracts §23): segment index in replay_segments / replay_sessions, blobs in S3,
// user / device / timeline from rum_events.

// ReplayFilter narrows GET /replays.
type ReplayFilter struct {
	App          string
	Q            string
	HasErrors    bool
	MinDurationS float64
	Limit        int
}

const replayCols = `service, session_id, min(start_ts) AS st, max(end_ts) AS en, toInt64(sum(segments)), toInt64(sum(clicks)),
    toInt64(sum(errors)) AS errs, toInt64(sum(rage_clicks)), any(platform), any(format), anyLastIf(user_id, user_id != ''),
    any(anonymous_id), argMinMerge(first_view)`

func scanReplays(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}) ([]model.ReplaySummary, error) {
	defer rows.Close()
	out := []model.ReplaySummary{}
	for rows.Next() {
		var r model.ReplaySummary
		var st, en time.Time
		var segs, clicks, errs, rage int64
		if err := rows.Scan(&r.App, &r.SessionID, &st, &en, &segs, &clicks, &errs, &rage, &r.Platform, &r.Format,
			&r.User.ID, &r.User.AnonymousID, &r.FirstView); err != nil {
			return nil, err
		}
		r.StartedAt, r.DurationMs = ms(st), float64(en.Sub(st).Milliseconds())
		r.Segments, r.Clicks, r.Errors, r.RageClicks = int(segs), int(clicks), int(errs), int(rage)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListReplays returns recorded sessions active in the window, newest first.
func (d *DB) ListReplays(ctx context.Context, q Q, f ReplayFilter) (model.ReplayList, error) {
	from, to := q.W.From, q.W.To
	where, args := "", []any{from.Add(-24 * time.Hour).UnixMilli(), to.UnixMilli()}
	if f.App != "" {
		where += " AND service = ?"
		args = append(args, f.App)
	}
	if t := strings.TrimSpace(f.Q); t != "" {
		where += ` AND (session_id = ? OR session_id IN (SELECT session_id FROM replay_segments WHERE {{tenant}}
		    AND start_ts >= fromUnixTimestamp64Milli(?) AND start_ts < fromUnixTimestamp64Milli(?)
		    AND (positionCaseInsensitive(user_id, ?) > 0 OR positionCaseInsensitive(anonymous_id, ?) > 0 OR positionCaseInsensitive(view_name, ?) > 0))
		  OR session_id IN (SELECT session_id FROM rum_events WHERE {{tenant}} AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?)
		    AND (positionCaseInsensitive(user_id, ?) > 0 OR positionCaseInsensitive(anonymous_id, ?) > 0 OR positionCaseInsensitive(view_name, ?) > 0)))`
		args = append(args, t, from.Add(-24*time.Hour).UnixMilli(), to.UnixMilli(), t, t, t, micros(from.Add(-24*time.Hour)), micros(to), t, t, t)
	}
	having, hargs := " HAVING en >= fromUnixTimestamp64Milli(?) AND st < fromUnixTimestamp64Milli(?)", []any{from.UnixMilli(), to.UnixMilli()}
	if q.Env != "" {
		having += " AND any(env) = ?"
		hargs = append(hargs, q.Env)
	}
	if f.HasErrors {
		having += ` AND (errs > 0 OR session_id IN (SELECT session_id FROM rum_events WHERE {{tenant}} AND type = 'error'
		  AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?)))`
		hargs = append(hargs, micros(from.Add(-24*time.Hour)), micros(to))
	}
	if f.MinDurationS > 0 {
		having += " AND dateDiff('millisecond', st, en) >= ?"
		hargs = append(hargs, int64(f.MinDurationS*1000))
	}
	args = append(append(args, hargs...), clampLimit(f.Limit, 50, 500))
	rows, err := d.ch.Query(ctx, `SELECT `+replayCols+` FROM replay_sessions
	  WHERE {{tenant}} AND start_ts >= fromUnixTimestamp64Milli(?) AND start_ts < fromUnixTimestamp64Milli(?)`+where+`
	  GROUP BY service, session_id`+having+` ORDER BY st DESC LIMIT ?`, args...)
	if err != nil {
		return model.ReplayList{}, err
	}
	list, err := scanReplays(rows)
	if err != nil {
		return model.ReplayList{}, err
	}
	if err := d.enrichReplays(ctx, list); err != nil {
		return model.ReplayList{}, err
	}
	return model.ReplayList{Replays: list}, nil
}

// enrichReplays adds user / device / view / error fields from rum_events (by session_id, around the recording).
func (d *DB) enrichReplays(ctx context.Context, list []model.ReplaySummary) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]string, len(list))
	appSet := map[string]bool{}
	lo, hi := time.UnixMilli(list[0].StartedAt), time.UnixMilli(list[0].StartedAt)
	for i, r := range list {
		ids[i] = r.SessionID
		appSet[r.App] = true
		st := time.UnixMilli(r.StartedAt)
		en := st.Add(time.Duration(r.DurationMs) * time.Millisecond)
		lo, hi = minT(lo, st), maxT(hi, en)
	}
	apps := make([]string, 0, len(appSet))
	for a := range appSet {
		apps = append(apps, a)
	}
	rows, err := d.ch.Query(ctx, `SELECT session_id, anyIf(user_id, user_id != ''), anyIf(anonymous_id, anonymous_id != ''), anyIf(country, country != ''),
	    anyIf(device_type, device_type != ''), anyIf(browser, browser != ''), anyIf(os, os != ''), anyIf(app_version, app_version != ''),
	    toInt64(countIf(type = 'view_start')), toInt64(countIf(type = 'error')), argMinIf(view_name, ts, type = 'view_start')
	  FROM rum_events WHERE {{tenant}} AND app IN (?) AND session_id IN (?) AND ts >= fromUnixTimestamp64Micro(?) AND ts <= fromUnixTimestamp64Micro(?)
	  GROUP BY session_id`, apps, ids, micros(lo.Add(-time.Hour)), micros(hi.Add(time.Hour)))
	if err != nil {
		return err
	}
	defer rows.Close()
	type rum struct {
		user, anon, country, device, browser, os, version, first string
		views, errors                                            int64
	}
	by := map[string]rum{}
	for rows.Next() {
		var id string
		var r rum
		if err := rows.Scan(&id, &r.user, &r.anon, &r.country, &r.device, &r.browser, &r.os, &r.version, &r.views, &r.errors, &r.first); err != nil {
			return err
		}
		by[id] = r
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range list {
		r, ok := by[list[i].SessionID]
		if !ok {
			continue
		}
		s := &list[i]
		if r.user != "" {
			s.User.ID = r.user
		}
		if s.User.AnonymousID == "" {
			s.User.AnonymousID = r.anon
		}
		s.Country, s.Device, s.Browser, s.OS, s.AppVersion = r.country, r.device, r.browser, r.os, r.version
		s.Views = int(r.views)
		s.Errors = max(s.Errors, int(r.errors))
		if r.first != "" {
			s.FirstView = r.first
		}
	}
	return nil
}

func minT(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxT(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// GetReplay returns one recorded session: summary, segment index and the RUM timeline (≤ 2000 events).
func (d *DB) GetReplay(ctx context.Context, sessionID string) (*model.ReplayDetail, error) {
	if sessionID == "" || len(sessionID) > 128 {
		return nil, ErrNotFound
	}
	rows, err := d.ch.Query(ctx, `SELECT segment, argMax(start_ts, received_at), argMax(end_ts, received_at), argMax(events, received_at),
	    argMax(bytes, received_at), argMax(has_full_snapshot, received_at)
	  FROM replay_segments WHERE {{tenant}} AND session_id = ? GROUP BY segment ORDER BY segment LIMIT 100000`, sessionID)
	if err != nil {
		return nil, err
	}
	out := &model.ReplayDetail{Segments: []model.ReplaySegment{}, Timeline: []model.TimelineItem{}}
	for rows.Next() {
		var s model.ReplaySegment
		var seg, events, bytes uint32
		var st, en time.Time
		var full uint8
		if err := rows.Scan(&seg, &st, &en, &events, &bytes, &full); err != nil {
			rows.Close()
			return nil, err
		}
		s.Segment, s.StartTs, s.EndTs, s.Events, s.Bytes, s.HasFullSnapshot = int(seg), ms(st), ms(en), int(events), int(bytes), full == 1
		out.Segments = append(out.Segments, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out.Segments) == 0 {
		return nil, ErrNotFound
	}
	srows, err := d.ch.Query(ctx, `SELECT `+replayCols+` FROM replay_sessions WHERE {{tenant}} AND session_id = ?
	  GROUP BY service, session_id ORDER BY st LIMIT 1`, sessionID)
	if err != nil {
		return nil, err
	}
	list, err := scanReplays(srows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 { // MV row not there (should not happen): build it from the segments
		first, last := out.Segments[0], out.Segments[len(out.Segments)-1]
		list = []model.ReplaySummary{{SessionID: sessionID, StartedAt: first.StartTs, DurationMs: float64(last.EndTs - first.StartTs), Segments: len(out.Segments)}}
	}
	if err := d.enrichReplays(ctx, list); err != nil {
		return nil, err
	}
	out.Summary = list[0]
	out.Timeline, err = d.replayTimeline(ctx, out.Summary)
	return out, err
}

func (d *DB) replayTimeline(ctx context.Context, s model.ReplaySummary) ([]model.TimelineItem, error) {
	st := time.UnixMilli(s.StartedAt)
	en := st.Add(time.Duration(s.DurationMs) * time.Millisecond)
	where, args := "", []any{s.SessionID, micros(st.Add(-30 * time.Minute)), micros(en.Add(30 * time.Minute))}
	if s.App != "" {
		where, args = " AND app = ?", append([]any{s.App}, args...)
	}
	rows, err := d.ch.Query(ctx, `SELECT toInt64(toUnixTimestamp64Milli(ts)), type, view_name, action_name, name, rage, dead, error_type, error_message,
	    toString(error_fingerprint), resource_method, resource_url, toInt64(status_code), lower(hex(trace_id)), duration_ms, vital_name, vital_value,
	    attrs['app_start.kind']
	  FROM rum_events WHERE {{tenant}}`+where+` AND session_id = ? AND ts >= fromUnixTimestamp64Micro(?) AND ts <= fromUnixTimestamp64Micro(?)
	    AND type IN ('view_start', 'action', 'error', 'resource', 'vital', 'app_start')
	  ORDER BY ts LIMIT 2000`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.TimelineItem{}
	for rows.Next() {
		var it model.TimelineItem
		var typ, view, action, name, errType, errMsg, fp, method, url, trace, vital, startKind string
		var rage, dead uint8
		var status int64
		var dur, vval float64
		if err := rows.Scan(&it.Ts, &typ, &view, &action, &name, &rage, &dead, &errType, &errMsg, &fp, &method, &url, &status, &trace, &dur,
			&vital, &vval, &startKind); err != nil {
			return nil, err
		}
		it.ViewName = view
		durp := func() {
			if dur > 0 {
				v := dur
				it.DurationMs = &v
			}
		}
		switch typ {
		case "view_start":
			it.Kind, it.Label = "view", view
			durp()
		case "action":
			it.Kind, it.Label = "action", action
			if it.Label == "" {
				it.Label = name
			}
			switch {
			case rage == 1:
				it.Detail = "rage click"
			case dead == 1:
				it.Detail = "dead click"
			}
		case "error":
			it.Kind, it.Label, it.Detail = "error", errType, errMsg
			if it.Label == "" {
				it.Label = "Error"
			}
			if fp != "0" {
				it.Fingerprint = fp
			}
		case "resource":
			it.Kind, it.Label, it.Status = "resource", strings.TrimSpace(method+" "+url), int(status)
			if strings.Trim(trace, "0") != "" {
				it.TraceID = trace
			}
			durp()
		case "vital":
			it.Kind, it.Label, it.Detail = "vital", vital, trimFloat(vval)
		case "app_start":
			it.Kind, it.Label = "vital", "App start"
			if startKind != "" {
				it.Label += " (" + startKind + ")"
			}
			durp()
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func trimFloat(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", v), "0"), ".")
}

// ReplaySegmentKey returns the S3 key of one segment (the latest delivery). ErrNotFound when the segment is unknown.
func (d *DB) ReplaySegmentKey(ctx context.Context, sessionID string, segment int) (string, error) {
	if sessionID == "" || segment < 0 {
		return "", ErrNotFound
	}
	var key string
	if err := d.ch.QueryRow(ctx, `SELECT argMax(blob_key, received_at) FROM replay_segments WHERE {{tenant}} AND session_id = ? AND segment = ?`,
		sessionID, uint32(segment)).Scan(&key); err != nil {
		return "", err
	}
	t, err := tq.MustTenant(ctx)
	if err != nil {
		return "", err
	}
	// keys are <org>/<project>/…; never serve another tenant's object even if an index row were wrong
	if key == "" || !strings.HasPrefix(key, fmt.Sprintf("%d/%d/", t.OrgID, t.ProjectID)) {
		return "", ErrNotFound
	}
	return key, nil
}

// replayedSessions returns which of ids have a replay (best effort: no table → none).
func (d *DB) replayedSessions(ctx context.Context, ids []string) map[string]bool {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out
	}
	rows, err := d.ch.Query(ctx, `SELECT DISTINCT session_id FROM replay_segments WHERE {{tenant}} AND session_id IN (?)`, ids)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// replayRef finds the newest event of evSQL (SELECT session_id, ts …, tenant filtered) that a replay covers.
// Best effort: errors (e.g. replay tables not migrated) return nil.
func (d *DB) replayRef(ctx context.Context, evSQL string, args []any) *model.ReplayRef {
	sql := `WITH ev AS (` + evSQL + `)
	  SELECT e.session_id, toInt64(toUnixTimestamp64Milli(e.ts)), toInt64(toUnixTimestamp64Milli(r.st))
	  FROM ev AS e INNER JOIN (SELECT session_id, min(start_ts) AS st, max(end_ts) AS en FROM replay_segments
	    WHERE {{tenant}} AND session_id IN (SELECT session_id FROM ev) GROUP BY session_id) AS r ON e.session_id = r.session_id
	  WHERE toUnixTimestamp64Milli(e.ts) >= toUnixTimestamp64Milli(r.st) - 5000 AND toUnixTimestamp64Milli(e.ts) <= toUnixTimestamp64Milli(r.en) + 5000
	  ORDER BY e.ts DESC LIMIT 1`
	rows, err := d.ch.Query(ctx, sql, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	if !rows.Next() {
		return nil
	}
	var id string
	var ts, st int64
	if rows.Scan(&id, &ts, &st) != nil {
		return nil
	}
	return &model.ReplayRef{SessionID: id, OffsetMs: max(0, ts-st)}
}

// rumErrorReplay: the latest occurrence of a RUM error fingerprint inside a replay.
func (d *DB) rumErrorReplay(ctx context.Context, q Q, app string, fp uint64) *model.ReplayRef {
	return d.replayRef(ctx, `SELECT session_id, ts FROM rum_events WHERE {{tenant}} AND app = ? AND type = 'error' AND error_fingerprint = ?
	    AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?) AND session_id != '' ORDER BY ts DESC LIMIT 500`,
		[]any{app, fp, micros(q.W.From), micros(q.W.To)})
}

// backendErrorReplay: a replay that recorded the browser request whose trace hit a backend error fingerprint.
func (d *DB) backendErrorReplay(ctx context.Context, q Q, service string, fp uint64) *model.ReplayRef {
	return d.replayRef(ctx, `SELECT session_id, ts FROM rum_events WHERE {{tenant}} AND type = 'resource'
	    AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?) AND session_id != ''
	    AND trace_id IN (SELECT trace_id FROM spans WHERE {{tenant}} AND service = ? AND error_fingerprint = ?
	      AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?) ORDER BY ts DESC LIMIT 500)
	  ORDER BY ts DESC LIMIT 500`,
		[]any{micros(q.W.From.Add(-time.Minute)), micros(q.W.To), service, fp, micros(q.W.From), micros(q.W.To)})
}
