package store

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

// vitalDefs are the UI's vitals with Web Vitals thresholds (good / poor).
var webVitals = []model.Vital{
	{ID: "lcp", Label: "LCP", Good: 2500, Poor: 4000, Unit: "ms"},
	{ID: "inp", Label: "INP", Good: 200, Poor: 500, Unit: "ms"},
	{ID: "cls", Label: "CLS", Good: 0.1, Poor: 0.25, Unit: "score"},
	{ID: "ttfb", Label: "TTFB", Good: 800, Poor: 1800, Unit: "ms"},
}

func (d *DB) isMobile(ctx context.Context, q Q, app string) (bool, error) {
	p, err := d.appPlatforms(ctx, Q{W: timerange.R7d.Window(q.Now), Now: q.Now})
	if err != nil {
		return false, err
	}
	pl := p[app]
	return pl != "" && pl != "web", nil
}

// vitalP75 returns p75 per vital name for an app (optionally one view) in w.
func (d *DB) vitalP75(ctx context.Context, w timerange.Window, env, app, view string) (map[string]float64, error) {
	ec, ea := envCond(env, "env")
	where, args := " AND app = ?", []any{app}
	if view != "" {
		where += " AND view_name = ?"
		args = append(args, view)
	}
	rows, err := d.ch.Query(ctx, `SELECT vital_name, toFloat64(quantilesTDigestMerge(0.5, 0.75, 0.95)(value)[2]) FROM rum_vitals_1m
	  WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)`+ec+where+` GROUP BY vital_name`,
		append(append([]any{secs(w.From), secs(w.To)}, ea...), args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var n string
		var v float64
		if err := rows.Scan(&n, &v); err != nil {
			return nil, err
		}
		out[strings.ToUpper(n)] = fin(v)
	}
	return out, rows.Err()
}

// AppSummary returns the headline numbers of a client app.
func (d *DB) AppSummary(ctx context.Context, q Q, app string) (model.AppSummary, error) {
	var s model.AppSummary
	mobile, err := d.isMobile(ctx, q, app)
	if err != nil {
		return s, err
	}
	cur, err := d.rumViews(ctx, q.W, q.Env, nil, " AND app = ?", []any{app}, false)
	if err != nil {
		return s, err
	}
	old, err := d.rumViews(ctx, q.W.Prev(), q.Env, nil, " AND app = ?", []any{app}, false)
	if err != nil {
		return s, err
	}
	rate := func(r rumRow) float64 {
		if mobile {
			return div(r.crashes, r.sessions)
		}
		return div(r.errors, r.views)
	}
	if len(cur) > 0 {
		r := cur[0]
		s.ActiveUsers, s.Sessions, s.PageViewsPerMin, s.ErrorRate = r.users, r.sessions, r.views/q.W.Minutes(), rate(r)
	}
	if len(old) > 0 {
		s.PrevActiveUsers, s.PrevErrorRate = old[0].users, rate(old[0])
	}
	ec, ea := envCond(q.Env, "env")
	row := d.ch.QueryRow(ctx, `SELECT toFloat64(avg(d)) FROM (SELECT session_id, dateDiff('millisecond', min(ts), max(ts)) / 1000 AS d
	  FROM rum_events WHERE {{tenant}} AND app = ? AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?)`+ec+` GROUP BY session_id)`,
		append([]any{app, micros(q.W.From), micros(q.W.To)}, ea...)...)
	if err := row.Scan(&s.AvgSessionSec); err != nil {
		return s, err
	}
	s.AvgSessionSec = fin(s.AvgSessionSec)
	vit, err := d.vitalP75(ctx, q.W, q.Env, app, "")
	if err != nil {
		return s, err
	}
	if !mobile {
		for _, v := range webVitals {
			v.Value = vit[v.Label]
			s.Vitals = append(s.Vitals, v)
		}
		return s, nil
	}
	starts, err := d.AppStarts(ctx, q, app)
	if err != nil {
		return s, err
	}
	p75 := map[string]float64{}
	for _, a := range starts {
		p75[a.Kind] = a.P75
	}
	apiP75, err := d.resourceP75(ctx, q, app)
	if err != nil {
		return s, err
	}
	s.Vitals = []model.Vital{
		{ID: "cold", Label: "Cold start", Value: p75["cold"], Good: 2000, Poor: 4000, Unit: "ms"},
		{ID: "warm", Label: "Warm start", Value: p75["warm"], Good: 1000, Poor: 2000, Unit: "ms"},
		{ID: "render", Label: "Slow frames", Value: vit["SLOW_FRAMES"], Good: 0.05, Poor: 0.1, Unit: "score"},
		{ID: "net", Label: "API p75", Value: apiP75, Good: 500, Poor: 1000, Unit: "ms"},
	}
	return s, nil
}

func (d *DB) resourceP75(ctx context.Context, q Q, app string) (float64, error) {
	ec, ea := envCond(q.Env, "env")
	row := d.ch.QueryRow(ctx, `SELECT toFloat64(quantile(0.75)(duration_ms)) FROM rum_events
	  WHERE {{tenant}} AND app = ? AND type = 'resource' AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?)`+ec,
		append([]any{app, micros(q.W.From), micros(q.W.To)}, ea...)...)
	var v float64
	err := row.Scan(&v)
	return fin(v), err
}

// RumPages returns pages (web) or screens (mobile) of an app.
func (d *DB) RumPages(ctx context.Context, q Q, app string) ([]model.RumPage, error) {
	rows, err := d.rumViews(ctx, q.W, q.Env, []string{"view_name"}, " AND app = ?", []any{app}, false)
	if err != nil {
		return nil, err
	}
	ec, ea := envCond(q.Env, "env")
	vrows, err := d.ch.Query(ctx, `SELECT view_name, vital_name, toFloat64(quantilesTDigestMerge(0.5, 0.75, 0.95)(value)[2]) FROM rum_vitals_1m
	  WHERE {{tenant}} AND app = ? AND minute >= toDateTime(?) AND minute < toDateTime(?)`+ec+` GROUP BY view_name, vital_name`,
		append([]any{app, secs(q.W.From), secs(q.W.To)}, ea...)...)
	if err != nil {
		return nil, err
	}
	vit := map[string]map[string]float64{}
	for vrows.Next() {
		var v, n string
		var x float64
		if err := vrows.Scan(&v, &n, &x); err != nil {
			vrows.Close()
			return nil, err
		}
		if vit[v] == nil {
			vit[v] = map[string]float64{}
		}
		vit[v][strings.ToUpper(n)] = fin(x)
	}
	vrows.Close()
	// bounce: sessions that entered on this view and saw only one view
	brows, err := d.ch.Query(ctx, `SELECT entry, toFloat64(count()), toFloat64(countIf(n = 1)) FROM (
	    SELECT session_id, argMin(view_name, ts) AS entry, count() AS n FROM rum_events
	    WHERE {{tenant}} AND app = ? AND type = 'view_start' AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?)`+ec+`
	    GROUP BY session_id) GROUP BY entry`, append([]any{app, micros(q.W.From), micros(q.W.To)}, ea...)...)
	if err != nil {
		return nil, err
	}
	bounce := map[string]float64{}
	for brows.Next() {
		var v string
		var n, one float64
		if err := brows.Scan(&v, &n, &one); err != nil {
			brows.Close()
			return nil, err
		}
		bounce[v] = div(one, n)
	}
	brows.Close()
	out := make([]model.RumPage, 0, len(rows))
	for _, r := range rows {
		v := r.key[0]
		if v == "" {
			continue
		}
		id := strings.ToLower(Slug(v))
		if id == "" {
			id = "root"
		}
		out = append(out, model.RumPage{
			ID: id, Path: v, Views: r.views, Users: r.users, AvgTime: div(r.tsum, r.views) / 1000,
			Lcp: vit[v]["LCP"], Inp: vit[v]["INP"], Cls: vit[v]["CLS"], ErrorRate: div(r.errors, r.views), Bounce: bounce[v],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Views > out[j].Views })
	return out, nil
}

// RumSessions returns the newest sessions of an app.
func (d *DB) RumSessions(ctx context.Context, q Q, app string, limit int) ([]model.Session, error) {
	ec, ea := envCond(q.Env, "env")
	rows, err := d.ch.Query(ctx, `SELECT session_id, anyIf(user_id, user_id != '') AS u, any(anonymous_id), any(country),
	    concat(any(device_type), if(any(os) != '', concat(' · ', any(os)), ''), if(any(browser) != '', concat(' · ', any(browser)), '')),
	    min(ts) AS started, toFloat64(dateDiff('millisecond', min(ts), max(ts)) / 1000), toFloat64(countIf(type = 'view_start')),
	    toFloat64(countIf(type = 'error')), argMinIf(view_name, ts, type = 'view_start'), argMaxIf(view_name, ts, type = 'view_start')
	  FROM rum_events WHERE {{tenant}} AND app = ? AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?)`+ec+`
	  GROUP BY session_id ORDER BY started DESC LIMIT ?`, append(append([]any{app, micros(q.W.From), micros(q.W.To)}, ea...), clampLimit(limit, 50, 500))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Session{}
	for rows.Next() {
		var s model.Session
		var user, anon string
		var started time.Time
		if err := rows.Scan(&s.ID, &user, &anon, &s.Country, &s.Device, &started, &s.Duration, &s.Pages, &s.Errors, &s.Entry, &s.Exit); err != nil {
			return nil, err
		}
		s.Identified = user != ""
		s.User = user
		if user == "" {
			s.User = anon
		}
		s.StartedAt = ms(started)
		out = append(out, s)
	}
	return out, rows.Err()
}

// RumErrors returns JS errors / crashes grouped by fingerprint.
func (d *DB) RumErrors(ctx context.Context, q Q, app string) ([]model.RumError, error) {
	ec, ea := envCond(q.Env, "env")
	rows, err := d.ch.Query(ctx, `SELECT toString(error_fingerprint), any(error_type), any(error_message), topK(1)(view_name)[1], toFloat64(count()) AS c,
	    toFloat64(uniq(session_id)), max(is_crash), min(ts), max(ts), argMax(app_version, ts)
	  FROM rum_events WHERE {{tenant}} AND app = ? AND type = 'error' AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?)`+ec+`
	  GROUP BY error_fingerprint ORDER BY c DESC LIMIT 200`, append([]any{app, micros(q.W.From), micros(q.W.To)}, ea...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.RumError{}
	for rows.Next() {
		var e model.RumError
		var crash uint8
		var first, last time.Time
		if err := rows.Scan(&e.Fingerprint, &e.Type, &e.Message, &e.View, &e.Count, &e.Sessions, &crash, &first, &last, &e.AppVersion); err != nil {
			return nil, err
		}
		e.Crash, e.FirstSeen, e.LastSeen = crash == 1, ms(first), ms(last)
		out = append(out, e)
	}
	return out, rows.Err()
}

// RumResources returns network calls made by an app (rum_resources_1m).
func (d *DB) RumResources(ctx context.Context, q Q, app string) ([]model.RumResource, error) {
	rows, err := d.ch.Query(ctx, `SELECT resource_url, method, toFloat64(sum(calls)) AS c, toFloat64(sum(errors)),
	    arrayMap(x -> toFloat64(x), quantilesTDigestMerge(0.5, 0.95)(duration))
	  FROM rum_resources_1m WHERE {{tenant}} AND app = ? AND minute >= toDateTime(?) AND minute < toDateTime(?)
	  GROUP BY resource_url, method ORDER BY c DESC LIMIT 200`, app, secs(q.W.From), secs(q.W.To))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.RumResource{}
	for rows.Next() {
		var r model.RumResource
		var c, e float64
		var qs []float64
		if err := rows.Scan(&r.URL, &r.Method, &c, &e, &qs); err != nil {
			return nil, err
		}
		r.CallsPerMin, r.ErrorRate, r.P50, r.P95 = c/q.W.Minutes(), div(e, c), qt(qs, 0), qt(qs, 1)
		out = append(out, r)
	}
	return out, rows.Err()
}

// AppStarts returns mobile app start timings by kind (cold | warm | hot; read from the event's name).
func (d *DB) AppStarts(ctx context.Context, q Q, app string) ([]model.AppStart, error) {
	ec, ea := envCond(q.Env, "env")
	rows, err := d.ch.Query(ctx, `SELECT attrs['app_start.kind'] AS k, toFloat64(count()),
	    arrayMap(x -> toFloat64(x), quantiles(0.5, 0.75, 0.95)(duration_ms))
	  FROM rum_events WHERE {{tenant}} AND app = ? AND type = 'app_start' AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?)`+ec+`
	  GROUP BY k ORDER BY k`, append([]any{app, micros(q.W.From), micros(q.W.To)}, ea...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.AppStart{}
	for rows.Next() {
		var a model.AppStart
		var qs []float64
		if err := rows.Scan(&a.Kind, &a.Count, &qs); err != nil {
			return nil, err
		}
		a.P50, a.P75, a.P95 = qt(qs, 0), qt(qs, 1), qt(qs, 2)
		out = append(out, a)
	}
	return out, rows.Err()
}

// AppAPICalls returns the backend endpoints an app calls (callers_1m with caller = app).
func (d *DB) AppAPICalls(ctx context.Context, q Q, app string) ([]model.AppAPICall, error) {
	ec, ea := envCond(q.Env, "env")
	rows, err := d.ch.Query(ctx, `SELECT service, method, route, toFloat64(sum(count)) AS c, toFloat64(sum(errors)),
	    arrayMap(x -> toFloat64(x), quantilesTDigestWeightedMerge(0.5, 0.95)(duration))
	  FROM callers_1m WHERE {{tenant}} AND caller = ? AND minute >= toDateTime(?) AND minute < toDateTime(?)`+ec+`
	  GROUP BY service, method, route ORDER BY c DESC LIMIT 200`, append([]any{app, secs(q.W.From), secs(q.W.To)}, ea...)...)
	if err != nil {
		return nil, err
	}
	type callRow struct {
		svc, method, route string
		c, e               float64
		qs                 []float64
	}
	var calls []callRow
	svcSet := map[string]bool{}
	for rows.Next() {
		var r callRow
		if err := rows.Scan(&r.svc, &r.method, &r.route, &r.c, &r.e, &r.qs); err != nil {
			rows.Close()
			return nil, err
		}
		calls = append(calls, r)
		svcSet[r.svc] = true
	}
	rows.Close()
	out := []model.AppAPICall{}
	if len(calls) == 0 {
		return out, nil
	}
	services, err := d.ListServices(ctx, q)
	if err != nil {
		return nil, err
	}
	svcBy := map[string]model.Service{}
	for _, s := range services {
		s.Spark = nil
		svcBy[s.ID] = s
	}
	names := make([]string, 0, len(svcSet))
	for s := range svcSet {
		names = append(names, s)
	}
	red, err := d.red(ctx, q.W, q.Env, []string{"service", "method", "route"}, " AND service IN (?)", []any{names}, false)
	if err != nil {
		return nil, err
	}
	eps := map[string]model.Endpoint{}
	for _, r := range red {
		eps[strings.Join(r.key, " ")] = model.Endpoint{
			ID: EndpointID(r.key[1], r.key[2]), ServiceID: r.key[0], Method: r.key[1], Route: r.key[2],
			Rpm: r.count / q.W.Minutes(), ErrorRate: div(r.errors, r.count), P50: qt(r.q, 0), P95: qt(r.q, 2), P99: qt(r.q, 3), Criticality: "low",
		}
	}
	for _, c := range calls {
		ep, ok := eps[c.svc+" "+c.method+" "+c.route]
		if !ok {
			ep = model.Endpoint{ID: EndpointID(c.method, c.route), ServiceID: c.svc, Method: c.method, Route: c.route, Criticality: "low"}
		}
		svc, ok := svcBy[c.svc]
		if !ok {
			svc = model.Service{ID: c.svc, Name: c.svc, Type: "http", Status: model.OK}
		}
		out = append(out, model.AppAPICall{Endpoint: ep, Service: svc, Rpm: c.c / q.W.Minutes(), ErrorRate: div(c.e, c.c), P95: qt(c.qs, 1)})
	}
	return out, nil
}

// RumSeries returns views/min, error rate and load p75 of an app (optionally one view) over time.
func (d *DB) RumSeries(ctx context.Context, q Q, app, view string) (model.SeriesSet, error) {
	where, args := " AND app = ?", []any{app}
	if view != "" {
		where += " AND view_name = ?"
		args = append(args, view)
	}
	rows, err := d.rumViews(ctx, q.W, q.Env, nil, where, args, true)
	if err != nil {
		return model.SeriesSet{}, err
	}
	b := timerange.NewBuilder(q.W, "views", "errorRate", "loadP75", "users", "crashes")
	for _, r := range rows {
		b.Set("views", r.bucket, r.views/q.W.StepMinutes())
		b.Set("errorRate", r.bucket, div(r.errors, r.views))
		b.Set("loadP75", r.bucket, qt(r.q, 1))
		b.Set("users", r.bucket, r.users)
		b.Set("crashes", r.bucket, r.crashes)
	}
	return labels(b, map[string]string{"views": "Views / min", "errorRate": "Error rate", "loadP75": "Load p75 (ms)", "users": "Users", "crashes": "Crashes"}), nil
}

// VitalSeries returns p75 of one vital (LCP, INP, CLS, TTFB, ...) over time.
func (d *DB) VitalSeries(ctx context.Context, q Q, app, vital, view string) (model.SeriesSet, error) {
	ec, ea := envCond(q.Env, "env")
	where, args := " AND app = ? AND upper(vital_name) = ?", []any{app, strings.ToUpper(vital)}
	if view != "" {
		where += " AND view_name = ?"
		args = append(args, view)
	}
	rows, err := d.ch.Query(ctx, `SELECT `+q.W.BucketExpr("minute")+` AS b, arrayMap(x -> toFloat64(x), quantilesTDigestMerge(0.5, 0.75, 0.95)(value))
	  FROM rum_vitals_1m WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)`+ec+where+` GROUP BY b`,
		append(append([]any{secs(q.W.From), secs(q.W.To)}, ea...), args...)...)
	if err != nil {
		return model.SeriesSet{}, err
	}
	defer rows.Close()
	b := timerange.NewBuilder(q.W, "p50", "p75", "p95")
	for rows.Next() {
		var bk int64
		var qs []float64
		if err := rows.Scan(&bk, &qs); err != nil {
			return model.SeriesSet{}, err
		}
		b.Set("p50", int(bk), qt(qs, 0))
		b.Set("p75", int(bk), qt(qs, 1))
		b.Set("p95", int(bk), qt(qs, 2))
	}
	return b.Build(), rows.Err()
}
