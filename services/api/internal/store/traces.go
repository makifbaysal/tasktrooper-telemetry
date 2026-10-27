package store

import (
	"context"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

// TraceFilter narrows trace search.
type TraceFilter struct {
	Service    string
	Ref        *EndpointRef
	ErrorsOnly bool
	MinMs      float64
	Slowest    bool
	Limit      int
	// Kubernetes / host placement (contracts §22): span resource_attrs k8s.pod.name, k8s.namespace.name,
	// k8s.node.name, host.name.
	Pod, Namespace, Node, Host string
}

// resourceConds returns the WHERE tail for the placement fields (resource attributes).
func (f TraceFilter) resourceConds() (string, []any) {
	where := ""
	var args []any
	for _, c := range []struct{ key, v string }{{"k8s.pod.name", f.Pod}, {"k8s.namespace.name", f.Namespace},
		{"k8s.node.name", f.Node}, {"host.name", f.Host}} {
		if c.v != "" {
			where += " AND resource_attrs[?] = ?"
			args = append(args, c.key, c.v)
		}
	}
	return where, args
}

// ValidTraceID reports whether s is a 32-char hex trace id.
func ValidTraceID(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func micros(t time.Time) int64 { return t.UnixMicro() }

// SearchTraces returns entry spans (one per request) matching f in the window.
func (d *DB) SearchTraces(ctx context.Context, q Q, f TraceFilter) ([]model.TraceSummary, error) {
	where := ""
	var args []any
	if f.Service != "" {
		where += " AND service = ?"
		args = append(args, f.Service)
	}
	if f.Ref != nil {
		where += " AND route = ?"
		args = append(args, f.Ref.Route)
		if f.Ref.Process {
			where += " AND kind = 'consumer'"
		} else {
			where += " AND http_method = ?"
			args = append(args, f.Ref.Method)
		}
	}
	if f.ErrorsOnly {
		where += " AND is_error = 1"
	}
	if f.MinMs > 0 {
		where += " AND duration_ns >= ?"
		args = append(args, uint64(f.MinMs*1e6))
	}
	rw, ra := f.resourceConds()
	where += rw
	args = append(args, ra...)
	ec, ea := envCond(q.Env, "env")
	order := "ts DESC"
	if f.Slowest {
		order = "duration_ns DESC"
	}
	sql := `SELECT lower(hex(trace_id)), toInt64(toUnixTimestamp64Milli(ts)), duration_ns / 1e6, toInt64(status_code),
	    service, http_method, route, is_error
	  FROM spans WHERE {{tenant}} AND is_entry = 1 AND ts >= fromUnixTimestamp64Micro(?) AND ts < fromUnixTimestamp64Micro(?)` + ec + where + `
	  ORDER BY ` + order + ` LIMIT ?`
	all := append(append(append([]any{micros(q.W.From), micros(q.W.To)}, ea...), args...), clampLimit(f.Limit, 20, 200))
	rows, err := d.ch.Query(ctx, sql, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.TraceSummary{}
	for rows.Next() {
		var t model.TraceSummary
		var st int64
		var isErr uint8
		if err := rows.Scan(&t.ID, &t.Ts, &t.Dur, &st, &t.Service, &t.Method, &t.Route, &isErr); err != nil {
			return nil, err
		}
		t.Status, t.IsError = int(st), isErr == 1
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTrace returns all spans of a trace, located through trace_index.
func (d *DB) GetTrace(ctx context.Context, id string) (*model.Trace, error) {
	if !ValidTraceID(id) {
		return nil, Invalid("trace id must be 32 hex chars")
	}
	var start, finish time.Time
	var services []string
	var spans, errs float64
	row := d.ch.QueryRow(ctx, `SELECT min(start), max(finish), groupUniqArrayArray(services), toFloat64(sum(spans)), toFloat64(sum(errors))
	  FROM trace_index WHERE {{tenant}} AND trace_id = unhex(?)`, id)
	if err := row.Scan(&start, &finish, &services, &spans, &errs); err != nil {
		return nil, err
	}
	if spans == 0 {
		return nil, ErrNotFound
	}
	rows, err := d.ch.Query(ctx, `SELECT lower(hex(span_id)), lower(hex(parent_span_id)), service, name, toString(kind), toInt64(toUnixTimestamp64Micro(ts)),
	    duration_ns, toInt64(status_code), is_error, dep, dep_kind, route, error_type, attrs
	  FROM spans WHERE {{tenant}} AND trace_id = unhex(?) AND ts >= fromUnixTimestamp64Micro(?) AND ts <= fromUnixTimestamp64Micro(?)
	  ORDER BY ts LIMIT 5000`, id, micros(start)-1, micros(finish)+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	t := &model.Trace{ID: id, Ts: start.UnixMilli(), Dur: float64(finish.Sub(start).Microseconds()) / 1000, Services: services, Errors: int(errs), Spans: []model.Span{}}
	sort.Strings(t.Services)
	startUs := micros(start)
	for rows.Next() {
		var s model.Span
		var tsUs, st int64
		var dur uint64
		var isErr uint8
		var depKind string
		if err := rows.Scan(&s.SpanID, &s.ParentSpanID, &s.Service, &s.Name, &s.Kind, &tsUs, &dur, &st, &isErr, &s.Dep, &depKind, &s.Route, &s.ErrorType, &s.Attrs); err != nil {
			return nil, err
		}
		if strings.Trim(s.ParentSpanID, "0") == "" {
			s.ParentSpanID = ""
		}
		s.Start = float64(tsUs-startUs) / 1000
		s.Dur = float64(dur) / 1e6
		s.Status, s.Failed = int(st), isErr == 1
		s.Category = "app"
		if depKind != "" {
			s.Category = depKind
		}
		t.Spans = append(t.Spans, s)
	}
	return t, rows.Err()
}

// ---------------------------------------------------------------- errors

// ErrorFilter narrows error issues.
type ErrorFilter struct {
	Service string
	Route   string
	Status  string
	Limit   int
}

// ListErrors returns error issues (errors_1h) with status from Postgres error_issues.
func (d *DB) ListErrors(ctx context.Context, q Q, f ErrorFilter) ([]model.ErrorIssue, error) {
	where := ""
	var args []any
	if f.Service != "" {
		where += " AND service = ?"
		args = append(args, f.Service)
	}
	having := ""
	if f.Route != "" {
		having = " HAVING r = ?"
	}
	sql := `SELECT service, toString(fingerprint), any(type), any(message), any(route) AS r, min(first_seen), max(last_seen),
	    toFloat64(sum(count)) AS c, lower(hex(any(sample_trace)))
	  FROM errors_1h WHERE {{tenant}} AND hour >= toStartOfHour(toDateTime(?)) AND hour < toDateTime(?)` + where + `
	  GROUP BY service, fingerprint` + having + ` ORDER BY c DESC LIMIT ?`
	all := append([]any{secs(q.W.From), secs(q.W.To)}, args...)
	if f.Route != "" {
		all = append(all, f.Route)
	}
	all = append(all, clampLimit(f.Limit, 50, 500))
	rows, err := d.ch.Query(ctx, sql, all...)
	if err != nil {
		return nil, err
	}
	var out []model.ErrorIssue
	for rows.Next() {
		var e model.ErrorIssue
		var first, last time.Time
		if err := rows.Scan(&e.Service, &e.Fingerprint, &e.Type, &e.Message, &e.Route, &first, &last, &e.Count, &e.SampleTrace); err != nil {
			rows.Close()
			return nil, err
		}
		if strings.Trim(e.SampleTrace, "0") == "" {
			e.SampleTrace = ""
		}
		e.FirstSeen, e.LastSeen, e.Status = ms(first), ms(last), "open"
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	status, err := d.errorStatuses(ctx)
	if err != nil {
		return nil, err
	}
	res := []model.ErrorIssue{}
	for _, e := range out {
		if s, ok := status[e.Fingerprint]; ok {
			e.Status = s
		}
		if f.Status == "" || f.Status == e.Status {
			res = append(res, e)
		}
	}
	return res, nil
}

func (d *DB) errorStatuses(ctx context.Context) (map[string]string, error) {
	rows, err := d.pg.Query(ctx, `SELECT fingerprint::text, status FROM error_issues WHERE {{tenant}}`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var f, s string
		if err := rows.Scan(&f, &s); err != nil {
			return nil, err
		}
		out[f] = s
	}
	return out, rows.Err()
}

// SetErrorStatus sets an issue's status (open | resolved | ignored).
func (d *DB) SetErrorStatus(ctx context.Context, fingerprint, status string) error {
	if _, err := strconv.ParseUint(fingerprint, 10, 64); err != nil {
		return Invalid("fingerprint must be an unsigned 64-bit integer")
	}
	if status != "open" && status != "resolved" && status != "ignored" {
		return Invalid("status must be open|resolved|ignored")
	}
	_, err := d.pg.Exec(ctx, `INSERT INTO error_issues (project_id, fingerprint, status, updated_at) VALUES ({{project}}, $1::numeric, $2, now())
	  ON CONFLICT (project_id, fingerprint) DO UPDATE SET status = excluded.status, updated_at = now()`, fingerprint, status)
	return err
}
