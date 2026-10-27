// Package ctl is loadtest-ctl (ADR-015): it claims queued runs, admits them (verified domain, plan limits,
// monthly VU-minutes), starts one runner per region, merges the runners' shard rows into the run's live
// view and final summary, handles aborts, and verifies target domains.
package ctl

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/tasktrooper/telemetry/services/loadtest/internal/obs"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/spec"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/store"
)

// Config of the controller.
type Config struct {
	Instance       string
	Limits         Limits
	DefaultRegions []string
	Poll           time.Duration // claim + supervise interval (default 1s)
	LiveEvery      time.Duration // live merge interval (default 2s)
	VerifyEvery    time.Duration // domain verification interval (default 10s)
	StartSlack     time.Duration // time a runner may take to start (default 2m)
	StaleAfter     time.Duration // other instances' runs without shard updates (default 3m)
	MaxConcurrent  int           // runs at once (default 4)
	// Projects restricts the controller to these projects (TT_LOADTEST_PROJECTS); empty = all.
	Projects []int64
}

// Controller is loadtest-ctl.
type Controller struct {
	cfg      Config
	st       *store.Store
	exec     Executor
	verifier *Verifier
	log      *slog.Logger
	metrics  *obs.Registry

	mu     sync.Mutex
	active map[int64]*activeRun
}

type activeRun struct {
	run        *store.Run
	plan       *spec.Plan
	handles    map[string]Handle
	exitedAt   map[string]time.Time
	stopping   bool
	ctlAbort   bool // the controller (gate / deadline) asked for the abort, not the user
	lastLive   time.Time
	deadline   time.Time
	hardStop   time.Time
	lastStatus time.Time
}

// New builds a controller.
func New(cfg Config, st *store.Store, ex Executor, v *Verifier, log *slog.Logger, m *obs.Registry) *Controller {
	if cfg.Poll <= 0 {
		cfg.Poll = time.Second
	}
	if cfg.LiveEvery <= 0 {
		cfg.LiveEvery = 2 * time.Second
	}
	if cfg.VerifyEvery <= 0 {
		cfg.VerifyEvery = 10 * time.Second
	}
	if cfg.StartSlack <= 0 {
		cfg.StartSlack = 2 * time.Minute
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 3 * time.Minute
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	if m == nil {
		m = obs.New()
	}
	return &Controller{cfg: cfg, st: st, exec: ex, verifier: v, log: log, metrics: m, active: map[int64]*activeRun{}}
}

// Recover marks this instance's running runs from a previous process as error: their runners' state is
// unknown (they also stop by themselves once they see the run is no longer running).
func (c *Controller) Recover(ctx context.Context) error {
	ids, err := c.st.RunningRuns(ctx, c.cfg.Instance)
	if err != nil {
		return err
	}
	for _, id := range ids {
		c.mu.Lock()
		_, mine := c.active[id]
		c.mu.Unlock()
		if mine {
			continue
		}
		if err := c.st.ErrorRun(ctx, id, "loadtest-ctl restarted while the run was in progress"); err != nil {
			return err
		}
		c.metrics.Add("tt_loadtest_runs_finished_total", 1, "state", "error")
		c.log.Warn("orphaned run marked error", "run", id)
	}
	return nil
}

// Run loops until ctx ends.
func (c *Controller) Run(ctx context.Context) error {
	if err := c.Recover(ctx); err != nil {
		return err
	}
	poll := time.NewTicker(c.cfg.Poll)
	defer poll.Stop()
	verify := time.NewTicker(c.cfg.VerifyEvery)
	defer verify.Stop()
	stale := time.NewTicker(30 * time.Second)
	defer stale.Stop()
	for {
		select {
		case <-ctx.Done():
			c.shutdown()
			return nil
		case <-poll.C:
			c.Supervise(ctx)
			for c.activeCount() < c.cfg.MaxConcurrent {
				ok, err := c.ClaimOnce(ctx)
				if err != nil {
					c.log.Error("claim", "err", err)
				}
				if !ok {
					break
				}
			}
		case <-verify.C:
			c.VerifyDomains(ctx)
		case <-stale.C:
			c.reapStale(ctx)
		}
	}
}

func (c *Controller) activeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.active)
}

// shutdown leaves runners running; a restarted controller marks the runs error (Recover).
func (c *Controller) shutdown() {
	c.log.Info("loadtest-ctl stopping", "active_runs", c.activeCount())
}

// ClaimOnce claims and starts at most one queued run. It reports whether a run was claimed.
func (c *Controller) ClaimOnce(ctx context.Context) (bool, error) {
	var plan *spec.Plan
	run, adm, err := c.st.ClaimNext(ctx, c.cfg.Instance, c.cfg.Projects, func(r store.Run) store.Admission {
		in := AdmitInput{Regions: c.cfg.DefaultRegions}
		var derr, uerr error
		in.Domains, derr = c.st.VerifiedDomains(ctx, r.ProjectID)
		in.UsedMin, uerr = c.st.UsageThisMonth(ctx, r.ProjectID)
		if err := errors.Join(derr, uerr); err != nil {
			return store.Admission{Reject: "internal error: " + err.Error()}
		}
		var a store.Admission
		a, plan = Admit(r.Spec, c.cfg.Limits, in)
		return a
	})
	if errors.Is(err, store.ErrNone) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if adm.Reject != "" {
		c.metrics.Add("tt_loadtest_runs_rejected_total", 1)
		c.log.Info("run rejected", "run", run.ID, "reason", adm.Reject)
		return true, nil
	}
	c.metrics.Add("tt_loadtest_runs_claimed_total", 1)
	dur := spec.Duration(plan.Stages)
	if plan.Script != "" && dur == 0 {
		dur = c.cfg.Limits.MaxDuration
	}
	a := &activeRun{run: run, plan: plan, handles: map[string]Handle{}, exitedAt: map[string]time.Time{}}
	a.deadline = time.Now().Add(dur + c.cfg.StartSlack + 30*time.Second)
	a.hardStop = a.deadline.Add(time.Minute)
	c.mu.Lock()
	c.active[run.ID] = a
	c.mu.Unlock()
	vuCap := 0
	if c.cfg.Limits.MaxVUs > 0 {
		vuCap = (c.cfg.Limits.MaxVUs + len(adm.Shards) - 1) / len(adm.Shards)
	}
	for _, sh := range adm.Shards {
		h, err := c.exec.Start(ctx, Job{RunID: run.ID, ProjectID: run.ProjectID, Region: sh.Region, Share: sh.Share, VUCap: vuCap,
			Deadline: dur + c.cfg.StartSlack + time.Minute})
		if err != nil {
			c.log.Error("start runner", "run", run.ID, "region", sh.Region, "err", err)
			_ = c.st.FailShard(ctx, run.ID, sh.Region, "runner did not start: "+err.Error())
			continue
		}
		a.handles[sh.Region] = h
	}
	c.log.Info("run started", "run", run.ID, "project", run.ProjectID, "regions", len(adm.Shards), "mode", plan.Mode,
		"peak_vus", plan.PeakVUs(), "duration", dur.String(), "vu_minutes", adm.VUMinutes)
	if len(a.handles) == 0 {
		c.stopAll(ctx, a) // every shard failed → the next Supervise finalizes as error
	}
	return true, nil
}

func (c *Controller) stopAll(ctx context.Context, a *activeRun) {
	if a.stopping {
		return
	}
	a.stopping = true
	for region, h := range a.handles {
		if err := h.Stop(ctx); err != nil {
			c.log.Warn("stop runner", "run", a.run.ID, "region", region, "err", err)
		}
	}
}

// Supervise advances every active run once: aborts, runner exits, deadlines, live merge, finalization.
func (c *Controller) Supervise(ctx context.Context) {
	c.mu.Lock()
	runs := make([]*activeRun, 0, len(c.active))
	for _, a := range c.active {
		runs = append(runs, a)
	}
	c.mu.Unlock()
	c.metrics.Set("tt_loadtest_runs_active", float64(len(runs)))
	for _, a := range runs {
		if err := c.supervise(ctx, a); err != nil && ctx.Err() == nil {
			c.log.Error("supervise", "run", a.run.ID, "err", err)
		}
	}
}

func (c *Controller) supervise(ctx context.Context, a *activeRun) error {
	id := a.run.ID
	state, abort, err := c.st.RunControl(ctx, id)
	if err != nil {
		return err
	}
	userAbort := (abort || state == "aborted") && !a.ctlAbort
	if state == "error" { // marked by someone else (stale reaper of another instance)
		c.stopAll(ctx, a)
		c.drop(id)
		return nil
	}
	if (abort || state == "aborted") && !a.stopping {
		c.log.Info("abort requested", "run", id)
		c.stopAll(ctx, a)
	}
	shards, err := c.st.Shards(ctx, id)
	if err != nil {
		return err
	}
	now := time.Now()
	// a gate abort in one region stops the others
	for _, sh := range shards {
		if sh.State == "aborted" && sh.AbortReason != "" && sh.AbortReason != "stopped" && sh.AbortReason != "stopped by user" && !abort {
			a.ctlAbort = true
			userAbort = false
			_ = c.st.RequestAbort(ctx, id)
			c.stopAll(ctx, a)
			break
		}
	}
	// runners that exited without a final row
	if now.Sub(a.lastStatus) >= 2*time.Second {
		a.lastStatus = now
		for region, h := range a.handles {
			exited, herr := h.Status(ctx)
			if !exited {
				continue
			}
			if _, ok := a.exitedAt[region]; !ok {
				a.exitedAt[region] = now
			}
			if now.Sub(a.exitedAt[region]) < 3*time.Second {
				continue // let a just-written final row land
			}
			for _, sh := range shards {
				if sh.Region == region && !sh.Terminal() {
					msg := "runner exited without a result"
					if herr != nil {
						msg = "runner exited: " + herr.Error()
					}
					_ = c.st.FailShard(ctx, id, region, msg)
				}
			}
		}
	}
	if now.After(a.deadline) && !a.stopping {
		c.log.Warn("run over deadline, stopping", "run", id)
		a.ctlAbort = true
		_ = c.st.RequestAbort(ctx, id)
		c.stopAll(ctx, a)
	}
	if now.After(a.hardStop) {
		for _, sh := range shards {
			if !sh.Terminal() {
				_ = c.st.FailShard(ctx, id, sh.Region, "runner did not finish in time")
			}
		}
	}
	if shards, err = c.st.Shards(ctx, id); err != nil {
		return err
	}
	done := len(shards) > 0
	for _, sh := range shards {
		done = done && sh.Terminal()
	}
	if done {
		f := MergeFinal(shards, a.plan, userAbort)
		var passed *bool
		if f.State != "error" {
			passed = &f.Passed
		}
		if err := c.st.FinishRun(ctx, id, f.State, f.Summary, passed, f.Error, f.VUMinutes); err != nil {
			return err
		}
		c.metrics.Add("tt_loadtest_runs_finished_total", 1, "state", f.State)
		c.log.Info("run finished", "run", id, "state", f.State, "requests", f.Summary.Requests, "error_rate", f.Summary.ErrorRate,
			"p95", f.Summary.P95, "vu_minutes", f.VUMinutes, "error", f.Error)
		c.stopAll(ctx, a)
		c.drop(id)
		return nil
	}
	if now.Sub(a.lastLive) >= c.cfg.LiveEvery {
		a.lastLive = now
		r := MergeLive(shards, a.plan)
		return c.st.UpdateLive(ctx, id, r.Live, r.Summary)
	}
	return nil
}

func (c *Controller) drop(id int64) {
	c.mu.Lock()
	delete(c.active, id)
	c.mu.Unlock()
}

func (c *Controller) reapStale(ctx context.Context) {
	ids, err := c.st.StaleRuns(ctx, c.cfg.StaleAfter, c.cfg.Projects)
	if err != nil {
		c.log.Error("stale runs", "err", err)
		return
	}
	for _, id := range ids {
		c.mu.Lock()
		_, mine := c.active[id]
		c.mu.Unlock()
		if mine {
			continue
		}
		if err := c.st.ErrorRun(ctx, id, "run lost: no runner updates for "+c.cfg.StaleAfter.String()); err == nil {
			c.log.Warn("stale run marked error", "run", id)
		}
	}
}

// VerifyDomains checks pending domain verification requests once.
func (c *Controller) VerifyDomains(ctx context.Context) {
	if c.verifier == nil {
		return
	}
	list, err := c.st.PendingVerifications(ctx, c.cfg.Projects)
	if err != nil {
		c.log.Error("pending verifications", "err", err)
		return
	}
	for _, d := range list {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		method, err := c.verifier.Check(cctx, d.Host, d.Token)
		cancel()
		if err != nil {
			c.metrics.Add("tt_loadtest_domain_checks_failed_total", 1)
			_ = c.st.MarkCheckFailed(ctx, d.ID, truncate(err.Error(), 500))
			c.log.Info("domain not verified", "domain", d.Host, "project", d.ProjectID, "err", err)
			continue
		}
		if err := c.st.MarkVerified(ctx, d.ID, method); err != nil {
			c.log.Error("mark verified", "err", err)
			continue
		}
		c.metrics.Add("tt_loadtest_domains_verified_total", 1, "method", method)
		c.log.Info("domain verified", "domain", d.Host, "project", d.ProjectID, "method", method)
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
