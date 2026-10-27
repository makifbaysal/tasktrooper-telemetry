// Package quotacheck is the alert-engine's AI quota threshold check (ADR-018, contracts §19): every Interval
// (5 min) it sums this month's ai_usage per org and, when an org reaches 80 % or 100 % of its effective limit,
// enqueues one notification (kind ai_quota, no incident) per orgs.ai_notify target. ai_quota_alerts dedupes per
// org + month + threshold, so any number of replicas may run the check.
package quotacheck

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tasktrooper/telemetry/services/pkg/aiquota"
)

// Org is one org's month: usage, limit inputs and notification targets.
type Org struct {
	ID        int64
	Plan      string
	Override  *int64
	Used      float64
	Targets   []string
	ProjectID int64 // any project of the org (channel / Slack installation lookup); 0 = none
}

// Alert is a threshold message to enqueue.
type Alert struct {
	Org       Org
	Limit     int64
	Threshold int   // highest crossed threshold (notified)
	Mark      []int // every crossed threshold (recorded so a jump to 100 % does not send 80 % later)
}

// Plan decides which orgs crossed a threshold (pure; dedupe happens in the database).
func Plan(orgs []Org, plans aiquota.Plans) []Alert {
	var out []Alert
	for _, o := range orgs {
		if len(o.Targets) == 0 {
			continue
		}
		limit := plans.OrgLimit(o.Plan, o.Override)
		crossed := aiquota.Crossed(o.Used, limit)
		if len(crossed) == 0 {
			continue
		}
		out = append(out, Alert{Org: o, Limit: limit, Threshold: crossed[len(crossed)-1], Mark: crossed})
	}
	return out
}

// Payload is notifications.payload of an ai_quota row.
type Payload struct {
	OrgID     int64   `json:"orgId"`
	ProjectID int64   `json:"projectId,omitempty"`
	Kind      string  `json:"kind"`
	Title     string  `json:"title"`
	Period    string  `json:"period"`
	Threshold int     `json:"threshold"`
	Used      float64 `json:"used"`
	Limit     int64   `json:"limit"`
	ResetsAt  int64   `json:"resetsAt"`
	Link      string  `json:"link,omitempty"`
}

// BuildPayload renders the payload of a for the month of now.
func BuildPayload(a Alert, now time.Time, publicURL string) Payload {
	p := Payload{OrgID: a.Org.ID, ProjectID: a.Org.ProjectID, Kind: "ai_quota", Period: aiquota.Period(now), Threshold: a.Threshold,
		Used: aiquota.Round2(a.Org.Used), Limit: a.Limit, ResetsAt: aiquota.ResetsAt(now).UnixMilli(),
		Title: fmt.Sprintf("AI usage at %d%% of this month's credits", a.Threshold)}
	if publicURL != "" {
		p.Link = publicURL + "/settings/usage"
	}
	return p
}

// DedupeKey of one target's notification.
func DedupeKey(orgID int64, period string, threshold int, target string) string {
	return "ai_quota:" + strconv.FormatInt(orgID, 10) + ":" + period + ":" + strconv.Itoa(threshold) + ":" + target
}

// Checker runs the periodic check.
type Checker struct {
	DB        *pgxpool.Pool
	Plans     aiquota.Plans
	Interval  time.Duration // default 5 min
	PublicURL string        // dashboard base for the Settings → Usage link
	Log       *slog.Logger
	Now       func() time.Time
}

// Run checks every Interval until ctx ends.
func (c *Checker) Run(ctx context.Context) {
	if c.Interval <= 0 {
		c.Interval = 5 * time.Minute
	}
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for {
		if n, err := c.Once(ctx); err != nil {
			if ctx.Err() == nil {
				c.log().Error("AI quota check failed", "err", err)
			}
		} else if n > 0 {
			c.log().Info("AI quota notifications enqueued", "orgs", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Checker) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// OrgsSQL lists orgs with usage this month and at least one notification target ($1 month start).
const OrgsSQL = `WITH u AS (
		SELECT org_id, sum(credits)::float8 AS used FROM ai_usage
		WHERE created_at >= $1 AND created_at < $1::timestamptz + interval '1 month' GROUP BY org_id)
	SELECT o.id, o.plan, o.ai_monthly_credits::bigint, u.used, o.ai_notify,
		coalesce((SELECT min(p.id) FROM projects p WHERE p.org_id = o.id), 0)
	FROM orgs o JOIN u ON u.org_id = o.id
	WHERE cardinality(o.ai_notify) > 0`

// Once runs one check; returns the number of orgs notified.
func (c *Checker) Once(ctx context.Context) (int, error) {
	now := c.now()
	month := aiquota.MonthStart(now)
	rows, err := c.DB.Query(ctx, OrgsSQL, month)
	if err != nil {
		return 0, err
	}
	orgs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Org, error) {
		var o Org
		err := r.Scan(&o.ID, &o.Plan, &o.Override, &o.Used, &o.Targets, &o.ProjectID)
		return o, err
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range Plan(orgs, c.Plans) {
		sent, err := c.enqueue(ctx, a, now, month)
		if err != nil {
			return n, fmt.Errorf("org %d: %w", a.Org.ID, err)
		}
		if sent {
			n++
		}
	}
	return n, nil
}

// enqueue records the crossed thresholds and, when the highest one is new, inserts one outbox row per target.
func (c *Checker) enqueue(ctx context.Context, a Alert, now, month time.Time) (bool, error) {
	sent := false
	err := pgx.BeginFunc(ctx, c.DB, func(tx pgx.Tx) error {
		sent = false
		for _, th := range a.Mark {
			tag, err := tx.Exec(ctx, `INSERT INTO ai_quota_alerts (org_id, period, threshold) VALUES ($1, $2, $3)
				ON CONFLICT DO NOTHING`, a.Org.ID, month, th)
			if err != nil {
				return err
			}
			if th == a.Threshold && tag.RowsAffected() == 1 {
				sent = true
			}
		}
		if !sent {
			return nil
		}
		p := BuildPayload(a, now, c.PublicURL)
		body, _ := json.Marshal(p)
		for _, target := range a.Targets() {
			if _, err := tx.Exec(ctx, `INSERT INTO notifications (incident_id, channel, kind, payload, dedupe_key)
				VALUES (NULL, $1, 'ai_quota', $2, $3) ON CONFLICT (dedupe_key) DO NOTHING`,
				target, body, DedupeKey(a.Org.ID, p.Period, a.Threshold, target)); err != nil {
				return err
			}
		}
		return nil
	})
	return sent, err
}

// Targets are the org's distinct non-empty notification targets.
func (a Alert) Targets() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range a.Org.Targets {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
