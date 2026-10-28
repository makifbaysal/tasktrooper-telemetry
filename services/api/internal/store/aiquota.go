package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
	"github.com/tasktrooper/telemetry/services/pkg/aiquota"
)

// AI usage quota (ADR-018, contracts §19). Usage is org scoped (not project scoped), so like the identity
// statements these run on the raw pool keyed by the org the handler authorized.

var (
	aiOnce sync.Once
	aiDef  *aiquota.Config
)

// aiConfig is DB.AIQuota or, when unset, the environment (TT_AI_PLAN_CREDITS, ...; defaults on a parse error).
func (d *DB) aiConfig() *aiquota.Config {
	if d.AIQuota != nil {
		return d.AIQuota
	}
	aiOnce.Do(func() {
		c, err := aiquota.FromEnv()
		if err != nil {
			c, _ = aiquota.Parse("", "", "")
		}
		aiDef = c
	})
	return aiDef
}

// pgxDB adapts the pool to aiquota.DB.
type pgxDB struct {
	q interface {
		QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	}
}

func (p pgxDB) Exec(context.Context, string, ...any) error { return errors.New("read only") }
func (p pgxDB) QueryRow(ctx context.Context, sql string, args ...any) aiquota.Row {
	return p.q.QueryRow(ctx, sql, args...)
}

// limitF renders an aiquota limit for JSON: nil = unlimited.
func limitF(limit int64) *float64 {
	if limit < 0 {
		return nil
	}
	v := float64(limit)
	return &v
}

func (d *DB) aiStatus(ctx context.Context, orgID, userID int64, month time.Time) (aiquota.Status, error) {
	var uid *int64
	if userID > 0 {
		uid = &userID
	}
	s, err := d.aiConfig().Usage(ctx, pgxDB{d.Pool}, orgID, uid, month)
	if errors.Is(err, aiquota.ErrOrgNotFound) {
		return s, ErrNotFound
	}
	return s, err
}

// MeAI is the `ai` field of GET /me: the org's and the user's usage this month (userID 0 = api key).
func (d *DB) MeAI(ctx context.Context, orgID, userID int64, now time.Time) (*model.MeAI, error) {
	s, err := d.aiStatus(ctx, orgID, userID, now)
	if err != nil {
		return nil, err
	}
	return &model.MeAI{Period: aiquota.Period(now), Limit: limitF(s.OrgLimit), Used: aiquota.Round2(s.OrgUsed),
		UserLimit: limitF(s.UserLimit), UserUsed: aiquota.Round2(s.UserUsed)}, nil
}

// AIUsage is GET /usage/ai for month (any instant in it). byUser is filled only when admin.
func (d *DB) AIUsage(ctx context.Context, orgID, userID int64, month time.Time, admin bool) (*model.AIUsage, error) {
	from := aiquota.MonthStart(month)
	to := aiquota.ResetsAt(from)
	s, err := d.aiStatus(ctx, orgID, userID, from)
	if err != nil {
		return nil, err
	}
	out := &model.AIUsage{Period: aiquota.Period(from), ResetsAt: to.UnixMilli(), Plan: s.Plan,
		Org:    model.AIOrgUsage{Limit: limitF(s.OrgLimit), Used: aiquota.Round2(s.OrgUsed)},
		Me:     model.AILimitUsed{Limit: limitF(s.UserLimit), Used: aiquota.Round2(s.UserUsed)},
		ByUser: []model.AIUserUsage{}, ByFeature: []model.AIFeatureUsage{}, ByProject: []model.AIProjectUsage{}}
	if s.Plan == "" {
		out.Plan = aiquota.PlanFree
	}
	if s.OrgLimit >= 0 {
		rem := aiquota.Round2(max(float64(s.OrgLimit)-s.OrgUsed, 0))
		out.Org.Remaining = &rem
	}
	if admin {
		q := &model.AIQuota{OrgID: idStr(orgID)}
		if err := d.Pool.QueryRow(ctx, `SELECT ai_monthly_credits::bigint, ai_notify FROM orgs WHERE id = $1`, orgID).Scan(&q.MonthlyCredits, &q.Notify); err != nil {
			return nil, err
		}
		if q.Notify == nil {
			q.Notify = []string{}
		}
		out.AIQuota = q
		rows, err := d.Pool.Query(ctx, `SELECT u.id, u.email, u.name, m.ai_monthly_credits::bigint, coalesce(sum(a.credits), 0)::float8
		  FROM memberships m JOIN users u ON u.id = m.user_id
		  LEFT JOIN ai_usage a ON a.org_id = m.org_id AND a.user_id = m.user_id AND a.created_at >= $2 AND a.created_at < $3
		  WHERE m.org_id = $1 GROUP BY u.id, u.email, u.name, m.ai_monthly_credits ORDER BY 5 DESC, u.email`, orgID, from, to)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var r model.AIUserUsage
			var id int64
			var capv *int64
			if err := rows.Scan(&id, &r.Email, &r.Name, &capv, &r.Used); err != nil {
				rows.Close()
				return nil, err
			}
			r.UserID, r.Limit, r.Used = idStr(id), limitF(aiquota.MemberLimit(capv)), aiquota.Round2(r.Used)
			out.ByUser = append(out.ByUser, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	rows, err := d.Pool.Query(ctx, `SELECT feature, sum(credits)::float8, count(*) FROM ai_usage
	  WHERE org_id = $1 AND created_at >= $2 AND created_at < $3 GROUP BY feature ORDER BY 2 DESC, 1`, orgID, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r model.AIFeatureUsage
		if err := rows.Scan(&r.Feature, &r.Credits, &r.Calls); err != nil {
			rows.Close()
			return nil, err
		}
		r.Credits = aiquota.Round2(r.Credits)
		out.ByFeature = append(out.ByFeature, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = d.Pool.Query(ctx, `SELECT coalesce(a.project_id, 0), coalesce(p.name, ''), sum(a.credits)::float8 FROM ai_usage a
	  LEFT JOIN projects p ON p.id = a.project_id
	  WHERE a.org_id = $1 AND a.created_at >= $2 AND a.created_at < $3 GROUP BY 1, 2 ORDER BY 3 DESC, 2`, orgID, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r model.AIProjectUsage
		var pid int64
		if err := rows.Scan(&pid, &r.Name, &r.Credits); err != nil {
			rows.Close()
			return nil, err
		}
		r.ProjectID, r.Credits = idStr(pid), aiquota.Round2(r.Credits)
		out.ByProject = append(out.ByProject, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	days := int(to.Sub(from).Hours() / 24)
	w := timerange.Window{Step: 24 * time.Hour, Points: days, From: from, To: to}
	b := timerange.NewBuilder(w, "credits")
	b.Label("credits", "Credits")
	rows, err = d.Pool.Query(ctx, `SELECT extract(day FROM created_at AT TIME ZONE 'UTC')::int, sum(credits)::float8 FROM ai_usage
	  WHERE org_id = $1 AND created_at >= $2 AND created_at < $3 GROUP BY 1`, orgID, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var day int
		var v float64
		if err := rows.Scan(&day, &v); err != nil {
			rows.Close()
			return nil, err
		}
		b.Set("credits", day-1, aiquota.Round2(v))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.Daily = b.Build()
	return out, nil
}

// memberAICredits returns each member's cap and usage this month (for GET /orgs/{org}/members).
func (d *DB) memberAICredits(ctx context.Context, orgID int64, now time.Time) (map[string]model.AILimitUsed, error) {
	from := aiquota.MonthStart(now)
	rows, err := d.Pool.Query(ctx, `SELECT m.user_id, m.ai_monthly_credits::bigint,
	    coalesce((SELECT sum(a.credits) FROM ai_usage a WHERE a.org_id = m.org_id AND a.user_id = m.user_id AND a.created_at >= $2 AND a.created_at < $3), 0)::float8
	  FROM memberships m WHERE m.org_id = $1`, orgID, from, aiquota.ResetsAt(from))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]model.AILimitUsed{}
	for rows.Next() {
		var id int64
		var capv *int64
		var used float64
		if err := rows.Scan(&id, &capv, &used); err != nil {
			return nil, err
		}
		out[idStr(id)] = model.AILimitUsed{Limit: limitF(aiquota.MemberLimit(capv)), Used: aiquota.Round2(used)}
	}
	return out, rows.Err()
}

// SetOrgAIQuota sets the org override (nil = plan default, -1 = unlimited) and the notification targets (nil = keep).
func (d *DB) SetOrgAIQuota(ctx context.Context, orgID int64, in model.AIQuotaInput) (*model.AIQuota, error) {
	if in.MonthlyCredits != nil && *in.MonthlyCredits < aiquota.Unlimited {
		return nil, Invalid("monthlyCredits must be null, -1 (unlimited) or >= 0")
	}
	if in.MonthlyCredits != nil && *in.MonthlyCredits > 1<<31-1 {
		return nil, Invalid("monthlyCredits is too large")
	}
	var notify []string
	if in.Notify != nil {
		notify = []string{}
		for _, n := range in.Notify {
			if n = strings.TrimSpace(n); n != "" {
				notify = append(notify, n)
			}
		}
		if len(notify) > 20 {
			return nil, Invalid("notify accepts at most 20 targets")
		}
	}
	out := &model.AIQuota{OrgID: idStr(orgID)}
	err := d.Pool.QueryRow(ctx, `UPDATE orgs SET ai_monthly_credits = $2, ai_notify = coalesce($3::text[], ai_notify) WHERE id = $1
	  RETURNING ai_monthly_credits::bigint, ai_notify`, orgID, in.MonthlyCredits, notify).Scan(&out.MonthlyCredits, &out.Notify)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if out.Notify == nil {
		out.Notify = []string{}
	}
	return out, err
}

// SetMemberAIQuota sets a member's monthly cap (nil = none).
func (d *DB) SetMemberAIQuota(ctx context.Context, orgID, userID int64, in model.MemberAIQuotaInput) (*model.MemberAIQuota, error) {
	if in.MonthlyCredits != nil && (*in.MonthlyCredits < 0 || *in.MonthlyCredits > 1<<31-1) {
		return nil, Invalid("monthlyCredits must be null (no cap) or a non-negative integer")
	}
	tag, err := d.Pool.Exec(ctx, `UPDATE memberships SET ai_monthly_credits = $3 WHERE org_id = $1 AND user_id = $2`, orgID, userID, in.MonthlyCredits)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return &model.MemberAIQuota{OrgID: idStr(orgID), UserID: idStr(userID), MonthlyCredits: in.MonthlyCredits}, nil
}
