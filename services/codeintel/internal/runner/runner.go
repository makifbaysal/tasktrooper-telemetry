// Package runner schedules analyses: every Interval it lists repositories, picks the due ones (never analyzed, or
// analyze_requested_at after analyzed_at), debounces push bursts, claims each in Postgres (one analysis per
// repository across replicas) and runs it with a timeout and panic recovery.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/analyze"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/obs"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/store"
)

// Store is the repository state the runner needs (implemented by store.Store).
type Store interface {
	Repos(ctx context.Context) ([]model.Repo, error)
	Claim(ctx context.Context, id int64, staleAfter time.Duration) (time.Time, []string, bool, error)
	FinishOK(ctx context.Context, id int64, started time.Time, sha string, languages, services []string) error
	FinishError(ctx context.Context, id int64, started time.Time, msg string, pending []string) error
}

// Analyzer runs one analysis (implemented by analyze.Analyzer).
type Analyzer interface {
	Run(ctx context.Context, repo model.Repo, pending []string) (*analyze.Report, error)
}

// Runner is the scheduling loop.
type Runner struct {
	Store       Store
	Analyzer    Analyzer
	Interval    time.Duration // tick (default 15 s)
	Debounce    time.Duration // wait this long after the last request before re-analyzing (default 30 s)
	Timeout     time.Duration // per analysis (default 10 min)
	Concurrency int           // parallel analyses (default 2)
	Backoff     time.Duration // after a failure, unless re-requested (default 5 min)
	Metrics     *obs.Registry
	Log         *slog.Logger
	Now         func() time.Time

	mu      sync.Mutex
	running map[int64]bool
	failed  map[int64]failure
	sem     chan struct{}
	wg      sync.WaitGroup
}

type failure struct {
	at        time.Time
	requested time.Time
}

func (r *Runner) defaults() {
	if r.Interval <= 0 {
		r.Interval = 15 * time.Second
	}
	if r.Debounce < 0 {
		r.Debounce = 0
	} else if r.Debounce == 0 {
		r.Debounce = 30 * time.Second
	}
	if r.Timeout <= 0 {
		r.Timeout = 10 * time.Minute
	}
	if r.Concurrency <= 0 {
		r.Concurrency = 2
	}
	if r.Backoff <= 0 {
		r.Backoff = 5 * time.Minute
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.Metrics == nil {
		r.Metrics = obs.NewRegistry()
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	r.mu.Lock()
	if r.running == nil {
		r.running, r.failed = map[int64]bool{}, map[int64]failure{}
		r.sem = make(chan struct{}, r.Concurrency)
	}
	r.mu.Unlock()
}

// Run ticks until ctx ends, then waits for running analyses.
func (r *Runner) Run(ctx context.Context) {
	r.defaults()
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		r.Tick(ctx)
		select {
		case <-ctx.Done():
			r.wg.Wait()
			return
		case <-t.C:
		}
	}
}

// Wait blocks until started analyses finish (tests).
func (r *Runner) Wait() { r.wg.Wait() }

// Tick starts analyses of due repositories (non-blocking).
func (r *Runner) Tick(ctx context.Context) {
	r.defaults()
	repos, err := r.Store.Repos(ctx)
	if err != nil {
		r.Log.Error("list repositories", "err", err)
		r.Metrics.Add("codeintel_errors_total", 1, "stage", "list")
		return
	}
	r.Metrics.Set("codeintel_repositories", float64(len(repos)))
	now := r.Now()
	for _, repo := range repos {
		if !r.ready(repo, now) {
			continue
		}
		select {
		case r.sem <- struct{}{}:
		default:
			return // all workers busy; next tick
		}
		r.mu.Lock()
		r.running[repo.ID] = true
		r.mu.Unlock()
		r.wg.Add(1)
		go func(repo model.Repo) {
			defer func() {
				<-r.sem
				r.mu.Lock()
				delete(r.running, repo.ID)
				r.mu.Unlock()
				r.wg.Done()
			}()
			r.analyze(ctx, repo)
		}(repo)
	}
}

// ready: due, not running here, quiet for Debounce after the last request, not in failure backoff.
func (r *Runner) ready(repo model.Repo, now time.Time) bool {
	if repo.Status == "disconnected" || !repo.Due() {
		return false
	}
	r.mu.Lock()
	running, f := r.running[repo.ID], r.failed[repo.ID]
	r.mu.Unlock()
	if running {
		return false
	}
	if repo.Status == "analyzing" && repo.AnalysisStartedAt != nil && now.Sub(*repo.AnalysisStartedAt) < 2*r.Timeout {
		return false // another replica holds it
	}
	req := time.Time{}
	if repo.AnalyzeRequestedAt != nil {
		req = *repo.AnalyzeRequestedAt
	}
	if !f.at.IsZero() && now.Sub(f.at) < r.Backoff && !req.After(f.requested) {
		return false
	}
	// Debounce pushes on analyzed repositories; a burst longer than 10× Debounce still gets analyzed.
	if repo.AnalyzedAt != nil && !req.IsZero() && now.Sub(req) < r.Debounce && now.Sub(*repo.AnalyzedAt) < 10*r.Debounce {
		return false
	}
	return true
}

func (r *Runner) analyze(ctx context.Context, repo model.Repo) {
	started, pending, ok, err := r.Store.Claim(ctx, repo.ID, 2*r.Timeout)
	if err != nil {
		r.Log.Error("claim repository", "repo", repo.FullName, "err", err)
		r.Metrics.Add("codeintel_errors_total", 1, "stage", "claim")
		return
	}
	if !ok {
		return
	}
	log := r.Log.With("repo", repo.FullName, "repo_id", repo.ID)
	actx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	t0 := time.Now()
	rep, err := r.safeRun(actx, repo, pending)
	dur := time.Since(t0)
	r.Metrics.Add("codeintel_analysis_seconds_total", dur.Seconds())

	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer fcancel()
	req := time.Time{}
	if repo.AnalyzeRequestedAt != nil {
		req = *repo.AnalyzeRequestedAt
	}
	if err != nil {
		restore := pending
		if !analyze.Incremental(repo, pending) {
			restore = []string{store.FullScan}
		}
		log.Warn("analysis failed", "err", err, "dur", dur.Round(time.Millisecond))
		r.Metrics.Add("codeintel_analyses_total", 1, "result", "error")
		r.mu.Lock()
		r.failed[repo.ID] = failure{at: r.Now(), requested: req}
		r.mu.Unlock()
		if ferr := r.Store.FinishError(fctx, repo.ID, started, err.Error(), restore); ferr != nil {
			log.Error("record analysis error", "err", ferr)
		}
		return
	}
	r.mu.Lock()
	delete(r.failed, repo.ID)
	r.mu.Unlock()
	if err := r.Store.FinishOK(fctx, repo.ID, started, rep.SHA, rep.Languages, rep.Services); err != nil {
		log.Error("record analysis", "err", err)
		return
	}
	r.Metrics.Add("codeintel_analyses_total", 1, "result", "ok")
	r.Metrics.Add("codeintel_items_total", float64(rep.Items))
	r.Metrics.Add("codeintel_llm_items_total", float64(rep.LLMItems))
	for _, w := range rep.Warnings {
		log.Warn("analysis warning", "warning", w)
	}
	log.Info("analysis done", "sha", rep.SHA, "incremental", rep.Incremental, "changed", rep.Changed, "items", rep.Items,
		"observed", rep.Observed, "shadows", rep.Shadows, "suggestions", rep.Suggestions, "scenarios", rep.Scenarios,
		"llm_items", rep.LLMItems, "monitors_created", rep.Monitors.Created, "monitors_updated", rep.Monitors.Updated,
		"monitors_deleted", rep.Monitors.Deleted, "dur", dur.Round(time.Millisecond))
}

// safeRun turns a panic in an extractor (or anywhere in the pipeline) into an analysis error.
func (r *Runner) safeRun(ctx context.Context, repo model.Repo, pending []string) (rep *analyze.Report, err error) {
	defer func() {
		if p := recover(); p != nil {
			r.Log.Error("analysis panic", "repo", repo.FullName, "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("internal error: %v", p)
		}
	}()
	return r.Analyzer.Run(ctx, repo, pending)
}
