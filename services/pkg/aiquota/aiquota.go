// Package aiquota is the AI usage quota (ADR-018, contracts §19): token → credit conversion with a configurable price
// table, plan credits, the UTC monthly period, and Reserve / Record over a tiny database interface (callers bring a
// pgx adapter). 1 credit = 0.01 USD of model cost. No external dependencies.
package aiquota

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Unlimited is the limit value meaning "no limit" (orgs.ai_monthly_credits = -1, plan enterprise).
const Unlimited int64 = -1

// LockClass is the first key of pg_advisory_xact_lock(LockClass, org_id) taken by Reserve.
const LockClass = 7140

// Features (ai_usage.feature).
const (
	FeatureCodeintelEnrich  = "codeintel.enrich"
	FeatureCodeintelSuggest = "codeintel.suggest"
	FeatureRCA              = "rca"
	FeatureOther            = "other"
)

// Denial reasons (Decision.Reason).
const (
	ReasonOrgLimit  = "org_limit"
	ReasonUserLimit = "user_limit"
)

// Plan names; an unknown plan counts as free.
const (
	PlanFree       = "free"
	PlanTeam       = "team"
	PlanEnterprise = "enterprise"
)

// Defaults of the environment variables.
const (
	DefaultPrice       = "3/15"
	DefaultPlanCredits = "free=100,team=5000,enterprise=-1"
)

// ---------------------------------------------------------------- prices

// Price is USD per million tokens.
type Price struct {
	In  float64
	Out float64
}

// Prices maps model ids to prices; Default is used for unknown models.
type Prices struct {
	Models  map[string]Price
	Default Price
}

// ParsePrice parses "<in>/<out>" (USD per million tokens).
func ParsePrice(s string) (Price, error) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return Price{}, fmt.Errorf("aiquota: price %q: want <in>/<out>", s)
	}
	in, err1 := strconv.ParseFloat(strings.TrimSpace(a), 64)
	out, err2 := strconv.ParseFloat(strings.TrimSpace(b), 64)
	if err1 != nil || err2 != nil || in < 0 || out < 0 || math.IsNaN(in) || math.IsNaN(out) || math.IsInf(in, 0) || math.IsInf(out, 0) {
		return Price{}, fmt.Errorf("aiquota: price %q: want non-negative numbers <in>/<out>", s)
	}
	return Price{In: in, Out: out}, nil
}

// ParsePrices parses TT_AI_PRICES ("<model>=<in>/<out>,...") with def as the default price ("" → DefaultPrice).
func ParsePrices(list, def string) (Prices, error) {
	if strings.TrimSpace(def) == "" {
		def = DefaultPrice
	}
	d, err := ParsePrice(def)
	if err != nil {
		return Prices{}, err
	}
	p := Prices{Models: map[string]Price{}, Default: d}
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		m, v, ok := strings.Cut(item, "=")
		if !ok || strings.TrimSpace(m) == "" {
			return Prices{}, fmt.Errorf("aiquota: TT_AI_PRICES entry %q: want <model>=<in>/<out>", item)
		}
		pr, err := ParsePrice(v)
		if err != nil {
			return Prices{}, err
		}
		p.Models[strings.TrimSpace(m)] = pr
	}
	return p, nil
}

// For returns the price of model: exact id, else the longest configured prefix (so "claude-x" prices
// "claude-x-20250101"), else Default.
func (p Prices) For(model string) Price {
	if pr, ok := p.Models[model]; ok {
		return pr
	}
	best, bestLen := p.Default, 0
	for m, pr := range p.Models {
		if len(m) > bestLen && strings.HasPrefix(model, m) {
			best, bestLen = pr, len(m)
		}
	}
	return best
}

// Tokens is the token usage of one call. Input excludes cache reads/writes (Anthropic usage semantics).
type Tokens struct {
	Input      int64
	CacheWrite int64 // billed at 1.25 × input price
	CacheRead  int64 // billed at 0.1 × input price
	Output     int64
}

// TotalInput is every prompt token (stored in ai_usage.input_tokens).
func (t Tokens) TotalInput() int64 { return t.Input + t.CacheWrite + t.CacheRead }

// Credits converts tokens of model to credits: (in × inPrice + out × outPrice) / 1e6 / 0.01.
func (p Prices) Credits(model string, inTok, outTok int64) float64 {
	return CreditsAt(p.For(model), inTok, outTok)
}

// CreditsFor converts a call's usage, pricing cache writes at 1.25× and cache reads at 0.1× of the input price.
func (p Prices) CreditsFor(model string, t Tokens) float64 {
	pr := p.For(model)
	usd := (float64(t.Input)*pr.In + float64(t.CacheWrite)*pr.In*1.25 + float64(t.CacheRead)*pr.In*0.1 + float64(t.Output)*pr.Out) / 1e6
	return round4(usd / 0.01)
}

// CreditsAt converts tokens at a price to credits (4 decimals, like ai_usage.credits).
func CreditsAt(pr Price, inTok, outTok int64) float64 {
	usd := (float64(inTok)*pr.In + float64(outTok)*pr.Out) / 1e6
	return round4(usd / 0.01)
}

// Estimate is the pre-call estimate: ceil(promptChars/4) × in + maxTokens × out, in credits.
func (p Prices) Estimate(model string, promptChars int, maxTokens int64) float64 {
	return CreditsAt(p.For(model), int64(math.Ceil(float64(promptChars)/4)), maxTokens)
}

func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }

// Round2 rounds credits for API responses (2 decimals).
func Round2(v float64) float64 { return math.Round(v*100) / 100 }

// ---------------------------------------------------------------- plans

// Plans maps plan names to monthly credits (Unlimited = no limit).
type Plans map[string]int64

// ParsePlans parses TT_AI_PLAN_CREDITS ("free=100,team=5000,enterprise=-1"); "" → DefaultPlanCredits. Plans missing
// from the list keep their default.
func ParsePlans(s string) (Plans, error) {
	p := Plans{PlanFree: 100, PlanTeam: 5000, PlanEnterprise: Unlimited}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, v, ok := strings.Cut(item, "=")
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if !ok || strings.TrimSpace(name) == "" || err != nil || n < Unlimited {
			return nil, fmt.Errorf("aiquota: TT_AI_PLAN_CREDITS entry %q: want <plan>=<credits> (-1 = unlimited)", item)
		}
		p[strings.ToLower(strings.TrimSpace(name))] = n
	}
	return p, nil
}

// Credits returns the plan's monthly credits; an unknown plan counts as free.
func (p Plans) Credits(plan string) int64 {
	if n, ok := p[strings.ToLower(strings.TrimSpace(plan))]; ok {
		return n
	}
	if n, ok := p[PlanFree]; ok {
		return n
	}
	return 100
}

// OrgLimit is the effective org limit: override ?? plan credits; any negative value = Unlimited.
func (p Plans) OrgLimit(plan string, override *int64) int64 {
	n := p.Credits(plan)
	if override != nil {
		n = *override
	}
	if n < 0 {
		return Unlimited
	}
	return n
}

// MemberLimit is the member cap: NULL or negative = Unlimited.
func MemberLimit(v *int64) int64 {
	if v == nil || *v < 0 {
		return Unlimited
	}
	return *v
}

// LimitPtr renders a limit for JSON (`limit: number|null`, null = unlimited).
func LimitPtr(limit int64) *int64 {
	if limit < 0 {
		return nil
	}
	return &limit
}

// Percent is used/limit × 100 (0 for Unlimited; +Inf-safe: a zero limit with usage counts as 100+).
func Percent(used float64, limit int64) float64 {
	switch {
	case limit < 0:
		return 0
	case limit == 0:
		if used > 0 {
			return 100
		}
		return 0
	}
	return used / float64(limit) * 100
}

// Thresholds are the notification thresholds (percent), highest last.
var Thresholds = []int{80, 100}

// Crossed returns the thresholds reached by used of limit, ascending (none for Unlimited).
func Crossed(used float64, limit int64) []int {
	if limit < 0 {
		return nil
	}
	pct := Percent(used, limit)
	var out []int
	for _, t := range Thresholds {
		if pct >= float64(t) {
			out = append(out, t)
		}
	}
	return out
}

// ---------------------------------------------------------------- period

// MonthStart is the first instant of t's UTC calendar month.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// ResetsAt is the start of the next UTC month (when the quota resets).
func ResetsAt(t time.Time) time.Time { return MonthStart(t).AddDate(0, 1, 0) }

// Period formats the month of t as "YYYY-MM".
func Period(t time.Time) string { return MonthStart(t).Format("2006-01") }

// ParsePeriod parses "YYYY-MM" into the month start (UTC).
func ParsePeriod(s string) (time.Time, error) {
	t, err := time.Parse("2006-01", strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, fmt.Errorf("aiquota: month %q: want YYYY-MM", s)
	}
	return t.UTC(), nil
}

// ---------------------------------------------------------------- config

// Config is the parsed environment.
type Config struct {
	Prices Prices
	Plans  Plans
	// Now is the clock (nil: time.Now).
	Now func() time.Time
}

// FromEnv reads TT_AI_PRICES, TT_AI_DEFAULT_PRICE and TT_AI_PLAN_CREDITS.
func FromEnv() (*Config, error) {
	return Parse(os.Getenv("TT_AI_PRICES"), os.Getenv("TT_AI_DEFAULT_PRICE"), os.Getenv("TT_AI_PLAN_CREDITS"))
}

// Parse builds a Config from the three variable values ("" → defaults).
func Parse(prices, defPrice, plans string) (*Config, error) {
	pr, err := ParsePrices(prices, defPrice)
	if err != nil {
		return nil, err
	}
	pl, err := ParsePlans(plans)
	if err != nil {
		return nil, err
	}
	return &Config{Prices: pr, Plans: pl}, nil
}

func (c *Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// ---------------------------------------------------------------- database

// Row is a single-row result (pgx.Row compatible).
type Row interface {
	Scan(dest ...any) error
}

// DB is the Postgres access aiquota needs. Callers adapt pgx (pool, conn or tx) to it. Reserve must get a
// transaction for its advisory lock to serialize checks of one org.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) error
	QueryRow(ctx context.Context, sql string, args ...any) Row
}

// ErrOrgNotFound is returned by Reserve for an unknown org.
var ErrOrgNotFound = errors.New("aiquota: org not found")

// Request asks to spend EstimateCredits for org (and user, when the work was started by a member).
type Request struct {
	OrgID           int64
	ProjectID       int64  // 0 = none
	UserID          *int64 // nil = automatic work (only the org limit applies)
	Feature         string
	EstimateCredits float64
}

// Decision is Reserve's answer. Limits are Unlimited (-1) when there is none.
type Decision struct {
	Allowed   bool
	Reason    string // "" | org_limit | user_limit
	OrgUsed   float64
	OrgLimit  int64
	UserUsed  float64
	UserLimit int64
}

// UsageSQL is the month's usage of an org and (with $3 non-NULL) of one of its members, plus the limits:
// $1 org id, $2 month start, $3 user id or NULL. Columns: plan, orgs.ai_monthly_credits, org used,
// memberships.ai_monthly_credits, user used. Reusable by the api.
const UsageSQL = `SELECT o.plan, o.ai_monthly_credits::bigint,
	(SELECT coalesce(sum(u.credits), 0)::float8 FROM ai_usage u WHERE u.org_id = o.id AND u.created_at >= $2 AND u.created_at < $2::timestamptz + interval '1 month'),
	(SELECT m.ai_monthly_credits::bigint FROM memberships m WHERE m.org_id = o.id AND m.user_id = $3::bigint),
	(SELECT coalesce(sum(u.credits), 0)::float8 FROM ai_usage u WHERE u.org_id = o.id AND u.user_id = $3::bigint AND u.created_at >= $2 AND u.created_at < $2::timestamptz + interval '1 month')
	FROM orgs o WHERE o.id = $1`

// Status is the quota state of an org (and member) for a month.
type Status struct {
	Plan      string
	OrgUsed   float64
	OrgLimit  int64
	UserUsed  float64
	UserLimit int64
}

// Usage reads the month's usage and effective limits (userID nil → only the org).
func (c *Config) Usage(ctx context.Context, db DB, orgID int64, userID *int64, month time.Time) (Status, error) {
	var s Status
	var orgOverride, memberCap *int64
	err := db.QueryRow(ctx, UsageSQL, orgID, MonthStart(month), userID).Scan(&s.Plan, &orgOverride, &s.OrgUsed, &memberCap, &s.UserUsed)
	if err != nil {
		if isNoRows(err) {
			return s, ErrOrgNotFound
		}
		return s, err
	}
	s.OrgLimit = c.Plans.OrgLimit(s.Plan, orgOverride)
	s.UserLimit = Unlimited
	if userID != nil {
		s.UserLimit = MemberLimit(memberCap)
	}
	return s, nil
}

// isNoRows recognizes pgx.ErrNoRows / sql.ErrNoRows without importing them.
func isNoRows(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no rows in result set")
}

// Reserve checks whether EstimateCredits fit this month's limits. Run it in a transaction: it takes
// pg_advisory_xact_lock(LockClass, org_id) so concurrent checks of one org are serialized (the limit is soft:
// a call may overshoot by its own estimate error, and concurrent callers by one call each).
// Denied when used + estimate > limit, org limit first, then the member cap (only with UserID).
func (c *Config) Reserve(ctx context.Context, tx DB, r Request) (Decision, error) {
	if err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::int, $2::int)`, LockClass, int32(r.OrgID)); err != nil {
		return Decision{}, fmt.Errorf("aiquota: lock: %w", err)
	}
	s, err := c.Usage(ctx, tx, r.OrgID, r.UserID, c.now())
	if err != nil {
		return Decision{}, err
	}
	return Decide(s, r.EstimateCredits), nil
}

// Decide applies the limits of s to an estimate.
func Decide(s Status, estimate float64) Decision {
	d := Decision{Allowed: true, OrgUsed: s.OrgUsed, OrgLimit: s.OrgLimit, UserUsed: s.UserUsed, UserLimit: s.UserLimit}
	if estimate < 0 {
		estimate = 0
	}
	const eps = 1e-9
	switch {
	case s.OrgLimit >= 0 && s.OrgUsed+estimate > float64(s.OrgLimit)+eps:
		d.Allowed, d.Reason = false, ReasonOrgLimit
	case s.UserLimit >= 0 && s.UserUsed+estimate > float64(s.UserLimit)+eps:
		d.Allowed, d.Reason = false, ReasonUserLimit
	}
	return d
}

// Usage is one LLM call to record.
type Usage struct {
	OrgID        int64
	ProjectID    int64  // 0 = NULL
	UserID       *int64 // nil = automatic work
	Feature      string
	Model        string
	InputTokens  int64 // every prompt token, cache reads/writes included
	OutputTokens int64
	Credits      float64
	Ref          string // e.g. "repo:12"
}

// InsertSQL writes one ai_usage row.
const InsertSQL = `INSERT INTO ai_usage (org_id, project_id, user_id, feature, model, input_tokens, output_tokens, credits, ref)
	VALUES ($1, nullif($2::bigint, 0), $3::bigint, $4, $5, $6, $7, $8, $9)`

// Record writes the real usage of a call (after it returned).
func Record(ctx context.Context, db DB, u Usage) error {
	if u.OrgID <= 0 {
		return errors.New("aiquota: record: org id required")
	}
	if u.Feature == "" {
		u.Feature = FeatureOther
	}
	return db.Exec(ctx, InsertSQL, u.OrgID, u.ProjectID, u.UserID, u.Feature, u.Model,
		clampInt32(u.InputTokens), clampInt32(u.OutputTokens), round4(u.Credits), u.Ref)
}

func clampInt32(v int64) int64 {
	if v < 0 {
		return 0
	}
	return min(v, math.MaxInt32)
}

// ---------------------------------------------------------------- formatting

// FormatCredits renders credits with thousands separators and no decimals ("4,012").
func FormatCredits(v float64) string {
	n := int64(math.Round(v))
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// SortedModels lists configured model ids (stable output for logs/docs).
func (p Prices) SortedModels() []string {
	out := make([]string, 0, len(p.Models))
	for m := range p.Models {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}
