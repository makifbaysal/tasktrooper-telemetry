// Package quota connects codeintel's LLM enrichment to the AI usage quota (ADR-018, contracts §19): a pgx adapter
// for services/pkg/aiquota and the per-analysis enrich.Budget (org + optional member, feature codeintel.enrich,
// ref repo:<id>).
package quota

import (
	"context"
	"log/slog"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tasktrooper/telemetry/services/pkg/aiquota"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/enrich"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

// Querier is the pgx surface the adapter needs (pool, conn or tx).
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PGX adapts a pgx querier to aiquota.DB.
type PGX struct{ Q Querier }

// Exec implements aiquota.DB.
func (p PGX) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := p.Q.Exec(ctx, sql, args...)
	return err
}

// QueryRow implements aiquota.DB.
func (p PGX) QueryRow(ctx context.Context, sql string, args ...any) aiquota.Row {
	return p.Q.QueryRow(ctx, sql, args...)
}

// Store is the database side of the gate: Reserve in a transaction, Record outside.
type Store interface {
	Reserve(ctx context.Context, cfg *aiquota.Config, r aiquota.Request) (aiquota.Decision, error)
	Record(ctx context.Context, u aiquota.Usage) error
}

// PGStore implements Store on a pool.
type PGStore struct{ Pool *pgxpool.Pool }

// Reserve runs aiquota.Reserve in its own short transaction (the advisory lock is released at commit).
func (s PGStore) Reserve(ctx context.Context, cfg *aiquota.Config, r aiquota.Request) (d aiquota.Decision, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		d, err = cfg.Reserve(ctx, PGX{Q: tx}, r)
		return err
	})
	return d, err
}

// Record implements Store.
func (s PGStore) Record(ctx context.Context, u aiquota.Usage) error {
	return aiquota.Record(ctx, PGX{Q: s.Pool}, u)
}

// Gate builds budgets per analysis.
type Gate struct {
	Store  Store
	Config *aiquota.Config
	Log    *slog.Logger
}

// For returns the budget of one repository analysis: org of the repository, member = analyze_requested_by.
func (g *Gate) For(repo model.Repo, feature string) enrich.Budget {
	if g == nil {
		return nil
	}
	log := g.Log
	if log == nil {
		log = slog.Default()
	}
	return &budget{g: g, log: log.With("repo_id", repo.ID, "org_id", repo.OrgID), req: aiquota.Request{
		OrgID: repo.OrgID, ProjectID: repo.ProjectID, UserID: repo.AnalyzeRequestedBy, Feature: feature,
	}, ref: "repo:" + strconv.FormatInt(repo.ID, 10)}
}

type budget struct {
	g   *Gate
	log *slog.Logger
	req aiquota.Request
	ref string

	mu     sync.Mutex
	denied string
}

// Reserve implements enrich.Budget. A failed check denies (fail closed: reason check_failed).
func (b *budget) Reserve(ctx context.Context, model string, estIn, estOut int64) (bool, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.denied != "" {
		return false, b.denied
	}
	r := b.req
	r.EstimateCredits = aiquota.CreditsAt(b.g.Config.Prices.For(model), estIn, estOut)
	d, err := b.g.Store.Reserve(ctx, b.g.Config, r)
	if err != nil {
		b.log.Warn("AI quota check failed; LLM step skipped", "err", err)
		b.denied = "check_failed"
		return false, b.denied
	}
	if !d.Allowed {
		b.denied = d.Reason
		b.log.Info("AI quota exceeded", "reason", d.Reason, "org_used", d.OrgUsed, "org_limit", d.OrgLimit,
			"user_used", d.UserUsed, "user_limit", d.UserLimit, "estimate", r.EstimateCredits)
		return false, d.Reason
	}
	return true, ""
}

// Record implements enrich.Budget; a failed write is logged (the call already happened).
func (b *budget) Record(ctx context.Context, model string, t enrich.Tokens) float64 {
	tok := aiquota.Tokens{Input: t.Input, CacheWrite: t.CacheWrite, CacheRead: t.CacheRead, Output: t.Output}
	credits := b.g.Config.Prices.CreditsFor(model, tok)
	err := b.g.Store.Record(context.WithoutCancel(ctx), aiquota.Usage{
		OrgID: b.req.OrgID, ProjectID: b.req.ProjectID, UserID: b.req.UserID, Feature: b.req.Feature, Model: model,
		InputTokens: tok.TotalInput(), OutputTokens: tok.Output, Credits: credits, Ref: b.ref,
	})
	if err != nil {
		b.log.Error("record AI usage", "err", err, "credits", credits)
	}
	return credits
}
