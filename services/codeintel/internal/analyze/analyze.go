// Package analyze is one repository analysis: fetch → extract → (incremental merge) → alert-as-code sync → traffic
// match → criticality (static rules, cached/fresh LLM) → monitored → inventory → suggestions → load test drafts.
package analyze

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"path"
	"sort"
	"strings"

	"github.com/tasktrooper/telemetry/services/pkg/aiquota"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/ascode"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/enrich"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/extract"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/observe"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/servicemap"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/source"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/store"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/suggest"
)

// Store is the persistence the analysis needs (implemented by store.Store).
type Store interface {
	Items(ctx context.Context, repoID int64) ([]model.Item, error)
	SaveItems(ctx context.Context, repo model.Repo, items []*model.Item, shadows []model.Item, services []string) error
	MonitorRefs(ctx context.Context, projectID int64) ([]observe.MonitorRef, error)
	Clusters(ctx context.Context, projectID int64) ([]suggest.Cluster, error)
	SaveSuggestions(ctx context.Context, repo model.Repo, list []suggest.Suggestion) (int, error)
	SaveScenarios(ctx context.Context, repo model.Repo, list []suggest.Scenario) error
	SyncCodeMonitors(ctx context.Context, repo model.Repo, mons []ascode.Monitor, broken []string) (store.SyncResult, error)
	Mappings(ctx context.Context, repoID int64) ([]servicemap.Mapping, error)
	SaveMappings(ctx context.Context, repo model.Repo, list []servicemap.Mapping) error
}

// Fetcher gets code onto disk (implemented by source.Fetcher).
type Fetcher interface {
	Fetch(ctx context.Context, repo model.Repo) (*source.Checkout, error)
}

// Differ lists files changed since an analyzed commit (implemented by source.Fetcher). Optional: a Fetcher without it
// keeps manual / overflow requests on a full scan.
type Differ interface {
	ChangedSince(ctx context.Context, co *source.Checkout, sha string) (*source.Diff, error)
}

// Changed-file sources of an incremental analysis (Report.DiffSource).
const (
	DiffWebhook = "webhook" // pending_paths from push webhooks
	DiffGit     = "git"     // git diff last_analyzed_sha..HEAD (manual request, webhook overflow)
)

// Analyzer runs analyses. Traffic and LLM are optional (nil).
type Analyzer struct {
	Store       Store
	Fetcher     Fetcher
	Traffic     observe.Source  // nil: ClickHouse not configured
	LLM         enrich.Enricher // nil: TT_ASSISTANT_API_KEY / TT_ASSISTANT_MODEL not set
	Quota       Budgets         // nil: no AI quota (unlimited)
	MaxLLMItems int             // per analysis (default 300)
	Log         *slog.Logger
}

// Budgets hands out the AI quota budget of one analysis (implemented by quota.Gate).
type Budgets interface {
	For(repo model.Repo, feature string) enrich.Budget
}

// Report summarizes one analysis.
type Report struct {
	SHA         string
	Incremental bool
	Changed     int    // files re-extracted (incremental)
	DiffSource  string // webhook | git ("" = full scan)
	FullReason  string // why an analyzed repository got a full scan (logs only)
	Languages   []string
	Services    []string
	Items       int
	Shadows     int
	Observed    int
	Suggestions int
	Scenarios   int
	LLMItems    int
	LLM         model.LLMStatus // ok | disabled | quota_exceeded (+ credits spent)
	Monitors    store.SyncResult
	Warnings    []string
	// Folders: repository folders with items; Unmatched: those no telemetry service fits (the dashboard asks).
	Folders, Unmatched int
	// FullScanNext: the folder → service mapping changed during an incremental analysis, so stored items of untouched
	// files still carry old service names; the analysis is recorded without a commit to force a full scan next.
	FullScanNext bool
}

// RecordedSHA is the commit FinishOK stores: "" when the next analysis must be a full scan.
func (r *Report) RecordedSHA() string {
	if r.FullScanNext {
		return ""
	}
	return r.SHA
}

// Incremental reports whether pending changed files allow an incremental analysis: the repository was analyzed
// before, the list is not a full-scan marker and not too long.
func Incremental(repo model.Repo, pending []string) bool {
	if len(pending) == 0 || len(pending) > store.MaxPendingPaths || repo.AnalyzedAt == nil || repo.LastAnalyzedSHA == "" {
		return false
	}
	for _, p := range pending {
		if p == store.FullScan {
			return false
		}
	}
	return true
}

// Run analyzes repo. pending is the changed-file list from the claim (empty → full scan).
func (a *Analyzer) Run(ctx context.Context, repo model.Repo, pending []string) (*Report, error) {
	log := a.Log
	if log == nil {
		log = slog.Default()
	}
	rep := &Report{}
	co, err := a.Fetcher.Fetch(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer co.Cleanup()
	rep.SHA = co.SHA

	prevMaps, err := a.Store.Mappings(ctx, repo.ID)
	if err != nil {
		return nil, fmt.Errorf("load service mappings: %w", err)
	}
	opt := extract.Options{RepoName: path.Base(repo.FullName), Overrides: servicemap.Overrides(prevMaps), Ignore: servicemap.IgnoredPaths(prevMaps)}
	changed := a.changedFiles(ctx, repo, pending, co, rep)
	if changed != nil {
		opt.Only = changed
		rep.Incremental, rep.Changed = true, len(changed)
	} else if rep.FullReason != "" {
		log.InfoContext(ctx, "full scan", "repo", repo.FullName, "reason", rep.FullReason)
	}
	res, err := extract.Repo(co.Dir, opt)
	if err != nil {
		return nil, fmt.Errorf("extract: %w", err)
	}
	rep.Languages, rep.Services = res.Languages, res.Services

	existing, err := a.Store.Items(ctx, repo.ID)
	if err != nil {
		return nil, fmt.Errorf("load inventory: %w", err)
	}
	prev := make(map[string]*model.Item, len(existing))
	for i := range existing {
		prev[existing[i].Identity()] = &existing[i]
	}

	// Fresh items: static criticality, then the cached LLM result when the code did not change.
	var items []*model.Item
	seen := map[string]bool{}
	for i := range res.Items {
		it := &res.Items[i]
		if it.CodeHash == "" {
			it.ComputeHash()
		}
		enrich.Static(it)
		if old := prev[it.Identity()]; old != nil && old.LLM != nil && old.CodeHash == it.CodeHash {
			enrich.Apply(it, old.LLM)
		}
		seen[it.Identity()] = true
		items = append(items, it)
	}
	// Incremental: items of untouched files stay as stored.
	if changed != nil {
		for i := range existing {
			old := &existing[i]
			if changed[old.File] || seen[old.Identity()] {
				continue
			}
			if old.LLM != nil {
				enrich.Apply(old, old.LLM) // otherwise the stored criticality stays
			}
			items = append(items, old)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Identity() < items[j].Identity() })
	rep.Items = len(items)

	// Alert-as-code (always read the whole directory: deletions must be seen).
	mons, broken := ascode.Load(co.Dir)
	var brokenPaths []string
	for _, b := range broken {
		brokenPaths = append(brokenPaths, b.Path)
		rep.Warnings = append(rep.Warnings, "monitor file "+b.Error())
	}
	if rep.Monitors, err = a.Store.SyncCodeMonitors(ctx, repo, mons, brokenPaths); err != nil {
		return nil, fmt.Errorf("alert-as-code: %w", err)
	}

	// Observed traffic, and the folders linked to the services it knows.
	var shadows []model.Item
	var tr *observe.Traffic
	if a.Traffic != nil {
		if tr, err = a.Traffic.Traffic(ctx, repo.OrgID, repo.ProjectID); err != nil {
			rep.Warnings = append(rep.Warnings, "traffic: "+err.Error())
			tr = nil
		}
	}
	var known map[string]bool // nil: telemetry unreadable, earlier automatic matches are kept
	if tr != nil {
		known = tr.Known()
	}
	maps := servicemap.Link(res.Roots, prevMaps, known)
	items = applyMappings(items, maps)
	rep.Services = serviceNames(items)
	for _, m := range maps {
		if m.Items > 0 {
			rep.Folders++
		}
		if m.Status == servicemap.Unmatched {
			rep.Unmatched++
		}
	}
	if changed != nil && servicemap.Changed(prevMaps, maps) {
		rep.FullScanNext = true
		rep.Warnings = append(rep.Warnings, "service mapping changed: the next analysis is a full scan")
	}
	if tr != nil {
		shadows = observe.Match(items, tr, rep.Services)
	} else {
		keepObserved(items, prev)
	}
	rep.Shadows = len(shadows)

	// LLM for new or changed code, most critical first; trivial endpoints (health, metrics…) keep their static rating.
	rep.LLM.Status = model.LLMDisabled
	if a.LLM != nil {
		rep.LLM.Status = model.LLMOK
		var todo []*model.Item
		for _, it := range items {
			if it.LLM == nil && it.Snippet != "" && it.Kind != model.KindDependency && it.Kind != model.KindView && !enrich.Trivial(it) {
				todo = append(todo, it)
			}
		}
		sort.SliceStable(todo, func(i, j int) bool {
			return model.CriticalityRank(todo[i].Criticality) > model.CriticalityRank(todo[j].Criticality)
		})
		limit := a.MaxLLMItems
		if limit <= 0 {
			limit = 300
		}
		if len(todo) > limit {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("llm: %d items over the per-analysis limit kept static criticality", len(todo)-limit))
			todo = todo[:limit]
		}
		if len(todo) > 0 {
			var budget enrich.Budget
			if a.Quota != nil {
				budget = a.Quota.For(repo, aiquota.FeatureCodeintelEnrich)
			}
			out, err := a.LLM.Enrich(ctx, repoSummary(repo, rep, items), todo, budget)
			if err != nil {
				rep.Warnings = append(rep.Warnings, "llm: "+err.Error())
			}
			rep.LLM = model.LLMStatus{Status: orDefault(out.Status, model.LLMOK), Reason: out.Reason, Credits: math.Round(out.Credits*1e4) / 1e4, Skipped: out.Skipped}
			if out.Status == model.LLMQuotaExceeded {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("llm: AI quota exceeded (%s); %d items kept static criticality", out.Reason, out.Skipped))
			}
			for _, it := range todo {
				if it.LLM != nil {
					rep.LLMItems++
				}
			}
		}
	}
	for _, it := range items {
		enrich.BumpForTraffic(it)
		if it.Observed {
			rep.Observed++
		}
	}

	refs, err := a.Store.MonitorRefs(ctx, repo.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("monitors: %w", err)
	}
	observe.MarkMonitored(items, refs)

	if err := a.Store.SaveItems(ctx, repo, items, shadows, rep.Services); err != nil {
		return nil, fmt.Errorf("save inventory: %w", err)
	}
	if err := a.Store.SaveMappings(ctx, repo, maps); err != nil {
		return nil, fmt.Errorf("save service mappings: %w", err)
	}
	clusters, err := a.Store.Clusters(ctx, repo.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("clusters: %w", err)
	}
	sugs := suggest.Build(items, clusters)
	if _, err := a.Store.SaveSuggestions(ctx, repo, sugs); err != nil {
		return nil, fmt.Errorf("save suggestions: %w", err)
	}
	rep.Suggestions = len(sugs)
	scen := suggest.LoadTests(items, repo.FullName, co.SHA)
	if err := a.Store.SaveScenarios(ctx, repo, scen); err != nil {
		return nil, fmt.Errorf("save load tests: %w", err)
	}
	rep.Scenarios = len(scen)
	return rep, nil
}

// changedFiles decides between incremental and full analysis. It returns the changed-file set (possibly empty: HEAD
// is the analyzed commit) or nil for a full scan, recording the source / full-scan reason in rep.
//
//   - never analyzed → full scan;
//   - a usable webhook list (Incremental) → that list;
//   - otherwise (manual request `{*}`, webhook overflow, empty list) → git diff last_analyzed_sha..HEAD when the
//     fetcher can diff; full scan when the base commit is gone (history rewrite), the diff is over MaxPendingPaths,
//     git fails, or the checkout is local.
//
// Either way a change to the service layout (`.trooper/config.yaml`, added/deleted module manifests) forces a full
// scan: untouched files keep their stored service otherwise.
func (a *Analyzer) changedFiles(ctx context.Context, repo model.Repo, pending []string, co *source.Checkout, rep *Report) map[string]bool {
	if repo.AnalyzedAt == nil || repo.LastAnalyzedSHA == "" {
		return nil
	}
	var paths []string
	if Incremental(repo, pending) {
		for _, p := range pending {
			// The webhook list does not say added vs modified: only the config file itself is a known layout change
			// ("M" never matches a manifest).
			if source.LayoutChange(p, "M") {
				rep.FullReason = "service layout changed: " + p
				return nil
			}
		}
		paths, rep.DiffSource = pending, DiffWebhook
	} else {
		d, ok := a.Fetcher.(Differ)
		if !ok {
			rep.FullReason = "fetcher cannot diff"
			return nil
		}
		diff, err := d.ChangedSince(ctx, co, repo.LastAnalyzedSHA)
		switch {
		case err != nil:
			rep.FullReason = "git diff: " + err.Error()
			return nil
		case len(diff.Paths) > store.MaxPendingPaths:
			rep.FullReason = fmt.Sprintf("git diff: %d files changed (> %d)", len(diff.Paths), store.MaxPendingPaths)
			return nil
		case len(diff.Layout) > 0:
			rep.FullReason = "service layout changed: " + strings.Join(diff.Layout, ", ")
			return nil
		}
		paths, rep.DiffSource = diff.Paths, DiffGit
	}
	changed := make(map[string]bool, len(paths))
	for _, p := range paths {
		if source.InTree(p) {
			changed[path.Clean(p)] = true
		}
	}
	return changed
}

// keepObserved carries stored observed/dead flags over when traffic is unavailable.
func keepObserved(items []*model.Item, prev map[string]*model.Item) {
	for _, it := range items {
		if old := prev[it.Identity()]; old != nil {
			it.Observed, it.Dead = old.Observed, old.Dead
		}
	}
}

// repoSummary is the LLM context: every inventory row (the enricher sends each batch only its service's rows, capped).
func repoSummary(repo model.Repo, rep *Report, items []*model.Item) enrich.RepoSummary {
	s := enrich.RepoSummary{Name: repo.FullName, Languages: rep.Languages, Services: rep.Services}
	for _, it := range items {
		s.Inventory = append(s.Inventory, strings.Join([]string{it.Kind, it.Service, it.Key}, " "))
	}
	return s
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Summary is the repositories.analysis jsonb of a finished analysis.
func (r *Report) Summary() *model.Analysis {
	return &model.Analysis{SHA: r.SHA, Incremental: r.Incremental, Items: r.Items, LLMItems: r.LLMItems, LLM: r.LLM}
}

// applyMappings gives fresh items the service of their folder (an automatic match found in this analysis may rename
// it) and drops items of ignored folders. Stored items (no Root) keep their service.
func applyMappings(items []*model.Item, maps []servicemap.Mapping) []*model.Item {
	by := make(map[string]servicemap.Mapping, len(maps))
	for _, m := range maps {
		by[m.Path] = m
	}
	kept := items[:0]
	for _, it := range items {
		if m, ok := by[it.Root]; ok && it.Root != "" {
			if m.Status == servicemap.Ignored {
				continue
			}
			if m.Service != "" {
				it.Service = m.Service
			}
		}
		kept = append(kept, it)
	}
	return kept
}

// serviceNames is the sorted set of item services.
func serviceNames(items []*model.Item) []string {
	set := map[string]bool{}
	for _, it := range items {
		set[it.Service] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
