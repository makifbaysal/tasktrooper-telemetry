package eval

import (
	"context"
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/rca"
)

// RCA windows: "now" is the last 15 minutes, baseline the hour before it (alerting.md § Kök neden analizi).
const (
	RCACurrent  = 15 * time.Minute
	RCABaseline = time.Hour
	deployLook  = 2 * time.Hour
)

// RCASource implements rca.Source over red_1m, edges_1m, callers_1m and deploys.
type RCASource struct {
	CH                Querier
	OrgID, ProjectID  uint32
	Env               string
	curFrom, baseFrom time.Time
	to                time.Time
}

// NewRCASource anchors the windows at now.
func NewRCASource(ch Querier, org, project uint32, env string, now time.Time) *RCASource {
	to := now.Truncate(time.Minute)
	cur := to.Add(-RCACurrent)
	return &RCASource{CH: ch, OrgID: org, ProjectID: project, Env: env, curFrom: cur, baseFrom: cur.Add(-RCABaseline), to: to}
}

func (s *RCASource) where(svcCol, svc string) *where {
	w := &where{parts: []string{"org_id = ?", "project_id = ?"}, args: []any{s.OrgID, s.ProjectID}}
	w.eq(svcCol, svc).eq("env", s.Env)
	return w.between("minute", s.baseFrom, s.to)
}

// split selects current-window and baseline-window sums of the given columns.
func (s *RCASource) split(cols ...string) (string, []any) {
	var sql string
	var args []any
	for i, c := range cols {
		if i > 0 {
			sql += ", "
		}
		sql += "sumIf(" + c + ", minute >= toDateTime(?)), sumIf(" + c + ", minute < toDateTime(?))"
		args = append(args, s.curFrom.Unix(), s.curFrom.Unix())
	}
	return sql, args
}

func (s *RCASource) Endpoint(ctx context.Context, service, route, method string) (rca.Pair, error) {
	sel, sargs := s.split("count", "errors", "duration_sum")
	w := s.where("service", service).eq("route", route).eq("method", method)
	var p rca.Pair
	err := s.CH.QueryRow(ctx, "SELECT "+sel+" FROM telemetry.red_1m WHERE "+w.sql(), append(sargs, w.args...)...).
		Scan(&p.Cur.Calls, &p.Base.Calls, &p.Cur.Errors, &p.Base.Errors, &p.Cur.DurSum, &p.Base.DurSum)
	return p, err
}

func (s *RCASource) Edges(ctx context.Context, service, route, method string) ([]rca.Edge, error) {
	sel, sargs := s.split("calls", "errors", "duration_sum")
	w := s.where("parent_service", service).eq("parent_route", route).eq("parent_method", method)
	rows, err := s.CH.Query(ctx, "SELECT dep, dep_kind, child_service, "+sel+" FROM telemetry.edges_1m WHERE "+w.sql()+
		" GROUP BY dep, dep_kind, child_service ORDER BY dep", append(sargs, w.args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rca.Edge
	for rows.Next() {
		var e rca.Edge
		if err := rows.Scan(&e.Dep, &e.DepKind, &e.ChildService, &e.Cur.Calls, &e.Base.Calls, &e.Cur.Errors, &e.Base.Errors, &e.Cur.DurSum, &e.Base.DurSum); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *RCASource) CalleeRoutes(ctx context.Context, callee, caller string) ([]rca.RouteCalls, error) {
	sel, sargs := s.split("count", "errors")
	w := s.where("service", callee).eq("caller", caller)
	rows, err := s.CH.Query(ctx, "SELECT route, method, "+sel+" FROM telemetry.callers_1m WHERE "+w.sql()+
		" GROUP BY route, method ORDER BY route", append(sargs, w.args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rca.RouteCalls
	for rows.Next() {
		var r rca.RouteCalls
		if err := rows.Scan(&r.Route, &r.Method, &r.Cur.Calls, &r.Base.Calls, &r.Cur.Errors, &r.Base.Errors); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *RCASource) Deploys(ctx context.Context, service string) ([]rca.Deploy, error) {
	w := &where{parts: []string{"org_id = ?", "project_id = ?"}, args: []any{s.OrgID, s.ProjectID}}
	w.eq("service", service).eq("env", s.Env)
	rows, err := s.CH.Query(ctx, "SELECT version, min(first_seen) AS at FROM telemetry.deploys WHERE "+w.sql()+
		" GROUP BY version HAVING at >= toDateTime(?) ORDER BY at", append(w.args, s.to.Add(-deployLook).Unix())...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rca.Deploy
	for rows.Next() {
		var d rca.Deploy
		if err := rows.Scan(&d.Version, &d.At); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
