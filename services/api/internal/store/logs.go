package store

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

// LogFilter narrows log search.
type LogFilter struct {
	Services []string
	Levels   []string // debug | info | warn | error (UI levels)
	Text     string
	TraceID  string
	Route    string
	Before   int64 // unix ms cursor (exclusive)
	Limit    int
}

// severities maps a UI level to ClickHouse severities.
var severities = map[string][]string{
	"debug": {"trace", "debug"}, "info": {"info"}, "warn": {"warn"}, "error": {"error", "fatal"},
}

// UILevel maps a ClickHouse severity to the UI level.
func UILevel(sev string) string {
	switch sev {
	case "trace", "debug":
		return "debug"
	case "fatal", "error":
		return "error"
	}
	return sev
}

var token = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// logWhere builds the WHERE tail for f (without the time range).
func logWhere(f LogFilter, withLevels bool) (string, []any, error) {
	var b strings.Builder
	var args []any
	if len(f.Services) > 0 {
		b.WriteString(" AND service IN (?)")
		args = append(args, f.Services)
	}
	if withLevels && len(f.Levels) > 0 {
		var sev []string
		for _, l := range f.Levels {
			s, ok := severities[l]
			if !ok {
				return "", nil, Invalid("level must be debug|info|warn|error")
			}
			sev = append(sev, s...)
		}
		b.WriteString(" AND toString(severity) IN (?)")
		args = append(args, sev)
	}
	if t := strings.TrimSpace(f.Text); t != "" {
		if token.MatchString(t) {
			b.WriteString(" AND hasToken(body, ?)") // tokenbf_v1 index
		} else {
			b.WriteString(" AND body ILIKE ?")
			t = "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(t) + "%"
		}
		args = append(args, t)
	}
	if f.TraceID != "" {
		if !ValidTraceID(f.TraceID) {
			return "", nil, Invalid("traceId must be 32 hex chars")
		}
		b.WriteString(" AND trace_id = unhex(?)")
		args = append(args, f.TraceID)
	}
	if f.Route != "" {
		b.WriteString(" AND route = ?")
		args = append(args, f.Route)
	}
	return b.String(), args, nil
}

// SearchLogs returns the newest lines matching f plus level/service facets.
func (d *DB) SearchLogs(ctx context.Context, q Q, f LogFilter) (model.LogPage, error) {
	where, args, err := logWhere(f, true)
	if err != nil {
		return model.LogPage{}, err
	}
	ec, ea := envCond(q.Env, "env")
	to := micros(q.W.To)
	if f.Before > 0 {
		to = min(to, f.Before*1000)
	}
	limit := clampLimit(f.Limit, 200, 1000)
	rows, err := d.ch.Query(ctx, `SELECT toInt64(toUnixTimestamp64Milli(ts)), service, toString(severity), body, lower(hex(trace_id)), route,
	    toString(cityHash64(ts, service, body))
	  FROM logs WHERE {{tenant}} AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?)`+ec+where+`
	  ORDER BY ts DESC LIMIT ?`, append(append(append([]any{micros(q.W.From), to}, ea...), args...), limit+1)...)
	if err != nil {
		return model.LogPage{}, err
	}
	page := model.LogPage{Lines: []model.LogLine{}, Levels: map[string]float64{}, Services: map[string]float64{}}
	for rows.Next() {
		var l model.LogLine
		var sev string
		if err := rows.Scan(&l.Ts, &l.Service, &sev, &l.Message, &l.TraceID, &l.Route, &l.ID); err != nil {
			rows.Close()
			return page, err
		}
		l.Level = UILevel(sev)
		if strings.Trim(l.TraceID, "0") == "" {
			l.TraceID = ""
		}
		page.Lines = append(page.Lines, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Lines) > limit {
		page.Lines = page.Lines[:limit]
		page.NextBefore = page.Lines[limit-1].Ts
	}
	// facets ignore the level filter (so every level can be toggled)
	fwhere, fargs, _ := logWhere(f, false)
	frows, err := d.ch.Query(ctx, `SELECT toString(severity), service, toFloat64(count()) FROM logs
	  WHERE {{tenant}} AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?)`+ec+fwhere+` GROUP BY severity, service`,
		append(append([]any{micros(q.W.From), micros(q.W.To)}, ea...), fargs...)...)
	if err != nil {
		return page, err
	}
	defer frows.Close()
	for frows.Next() {
		var sev, svc string
		var n float64
		if err := frows.Scan(&sev, &svc, &n); err != nil {
			return page, err
		}
		page.Levels[UILevel(sev)] += n
		page.Services[svc] += n
	}
	return page, frows.Err()
}

// LogSeries returns line counts per level over time.
func (d *DB) LogSeries(ctx context.Context, q Q, f LogFilter) (model.SeriesSet, error) {
	where, args, err := logWhere(f, true)
	if err != nil {
		return model.SeriesSet{}, err
	}
	ec, ea := envCond(q.Env, "env")
	rows, err := d.ch.Query(ctx, `SELECT toString(severity), `+q.W.BucketExpr("ts")+` AS b, toFloat64(count()) FROM logs
	  WHERE {{tenant}} AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?)`+ec+where+` GROUP BY severity, b`,
		append(append([]any{micros(q.W.From), micros(q.W.To)}, ea...), args...)...)
	if err != nil {
		return model.SeriesSet{}, err
	}
	defer rows.Close()
	b := timerange.NewBuilder(q.W, "error", "warn", "info", "debug")
	for rows.Next() {
		var sev string
		var bk int64
		var n float64
		if err := rows.Scan(&sev, &bk, &n); err != nil {
			return model.SeriesSet{}, err
		}
		b.Add(UILevel(sev), int(bk), n)
	}
	return b.Build(), rows.Err()
}

// LogPatterns returns Drain log templates (log_patterns_1m) ordered by count.
func (d *DB) LogPatterns(ctx context.Context, q Q, f LogFilter) ([]model.LogPattern, error) {
	where := ""
	var args []any
	if len(f.Services) > 0 {
		where += " AND service IN (?)"
		args = append(args, f.Services)
	}
	if len(f.Levels) > 0 {
		var sev []string
		for _, l := range f.Levels {
			sev = append(sev, severities[l]...)
		}
		where += " AND severity IN (?)"
		args = append(args, sev)
	}
	rows, err := d.ch.Query(ctx, `SELECT toString(pattern_id), any(severity), any(sample), groupUniqArray(service), toFloat64(sum(count)) AS c, max(minute)
	  FROM log_patterns_1m WHERE {{tenant}} AND minute >= toDateTime(?) AND minute < toDateTime(?)`+where+`
	  GROUP BY pattern_id ORDER BY c DESC LIMIT ?`, append(append([]any{secs(q.W.From), secs(q.W.To)}, args...), clampLimit(f.Limit, 100, 500))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.LogPattern{}
	for rows.Next() {
		var p model.LogPattern
		var sev string
		var last time.Time
		if err := rows.Scan(&p.ID, &sev, &p.Pattern, &p.Services, &p.Count, &last); err != nil {
			return nil, err
		}
		sort.Strings(p.Services)
		p.Level, p.Last = UILevel(sev), ms(last)
		out = append(out, p)
	}
	return out, rows.Err()
}
