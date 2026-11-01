// Package runner is loadtest-runner: it executes one region shard of a run (engine or k6 script), writes
// its shard row every tick and exports OTLP metrics. loadtest-ctl merges the shards into the run.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/tasktrooper/telemetry/services/loadtest/internal/engine"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/k6"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/otlpx"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/selftel"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/spec"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/store"
)

// Config of one shard.
type Config struct {
	RunID         int64
	Region        string
	Share         float64
	VUCap         int
	Tick          time.Duration
	Poll          time.Duration // abort polling (default 1s)
	MaxDuration   time.Duration // script mode timeout
	IngestURL     string
	DSN           string
	InternalToken string
	K6Binary      string
	Version       string
}

// Run executes the shard until done, aborted or ctx (SIGTERM) ends. Self telemetry (ADR-027): one
// loadtest.run span (child of the ctl claim span when ctx carries TRACEPARENT) plus loadtest.stage spans; never a
// span per generated request, and the target requests are not instrumented (they carry their own traceparent).
func Run(ctx context.Context, st *store.Store, cfg Config, log *slog.Logger) (err error) {
	if cfg.Region == "" {
		cfg.Region = "local"
	}
	ctx, span := selftel.StartSpan(ctx, "loadtest.run", attribute.Int64("tt.loadtest.run_id", cfg.RunID),
		attribute.String("tt.loadtest.region", cfg.Region), attribute.Float64("tt.loadtest.share", cfg.Share),
		attribute.Int("tt.loadtest.vu_cap", cfg.VUCap))
	defer func() { selftel.End(span, err) }()
	if cfg.Poll <= 0 {
		cfg.Poll = time.Second
	}
	run, err := st.GetRun(ctx, cfg.RunID)
	if err != nil {
		return fmt.Errorf("load run %d: %w", cfg.RunID, err)
	}
	// final writes must survive SIGTERM
	finish := func(state string, stats *engine.Stats, sum any, msg, reason string) error {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		var s any
		if stats != nil {
			s = stats
		}
		return st.ShardFinish(fctx, cfg.RunID, cfg.Region, state, s, sum, msg, reason)
	}
	span.SetAttributes(attribute.Int64("tt.project_id", run.ProjectID))
	if run.State != "running" {
		return fmt.Errorf("run %d is %s, not running", run.ID, run.State)
	}
	plan, err := spec.Parse(run.Spec)
	if err != nil {
		_ = finish("error", nil, nil, err.Error(), "")
		return err
	}
	if err := st.ShardStart(ctx, cfg.RunID, cfg.Region); err != nil {
		return err
	}
	log = log.With("run", cfg.RunID, "region", cfg.Region)
	span.SetAttributes(attribute.String("tt.loadtest.mode", plan.Mode), attribute.Int("tt.loadtest.peak_vus", plan.PeakVUs()),
		attribute.Float64("tt.loadtest.duration_s", spec.Duration(plan.Stages).Seconds()), attribute.Bool("tt.loadtest.script", plan.Script != ""))
	pollCtx := selftel.Untraced(ctx) // per-second / per-tick Postgres polls stay untraced

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var abortFn atomic.Pointer[func(string)]
	go func() { // stop on abort_requested_at / external state change
		t := time.NewTicker(cfg.Poll)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				state, abort, err := st.RunControl(selftel.Untraced(runCtx), cfg.RunID)
				if err != nil {
					continue
				}
				if abort || state != "running" {
					reason := "stopped by user"
					if state == "error" {
						reason = "run failed"
					}
					if f := abortFn.Load(); f != nil {
						(*f)(reason)
					} else {
						cancel()
					}
					return
				}
			}
		}
	}()

	if plan.Script != "" {
		return runScript(runCtx, plan, cfg, finish, log)
	}

	exp, err := otlpx.Config(cfg.IngestURL, cfg.DSN, cfg.InternalToken, run.ProjectID)
	if err != nil {
		log.Warn("metrics export disabled", "err", err)
	}
	if exp != nil {
		exp.RunID, exp.Region, exp.Version = fmt.Sprint(cfg.RunID), cfg.Region, cfg.Version
	}
	lastTick := time.Now()
	e := engine.New(engine.Options{Plan: plan, RunID: fmt.Sprint(cfg.RunID), Share: cfg.Share, VUCap: cfg.VUCap, Tick: cfg.Tick,
		OnTick: func(tk engine.Tick) {
			now := time.Now()
			from := lastTick
			lastTick = now
			if exp != nil && !tk.Final {
				go func(w *engine.Stats, vus float64) {
					ectx, c := context.WithTimeout(context.WithoutCancel(pollCtx), 10*time.Second)
					defer c()
					if err := exp.Export(ectx, w, vus, from, now); err != nil {
						log.Warn("metrics export", "err", err)
					}
				}(tk.Window, tk.Live.Vus)
			}
			if tk.Final {
				return
			}
			uctx, c := context.WithTimeout(context.WithoutCancel(pollCtx), 5*time.Second)
			defer c()
			if err := st.ShardUpdate(uctx, cfg.RunID, cfg.Region, tk.Live, tk.Total); err != nil {
				log.Warn("shard update", "err", err)
			}
		}})
	ab := e.Abort
	abortFn.Store(&ab)
	log.Info("shard starting", "mode", plan.Mode, "steps", len(plan.Steps), "share", cfg.Share, "duration", spec.Duration(plan.Stages).String())
	started := time.Now()
	res := e.Run(runCtx) // ctx (SIGTERM) → "stopped"; abortFn → user/gate reasons
	stageSpans(ctx, plan.Stages, started, time.Now())
	sum, _ := engine.Summarize(res.Stats, plan.Thresholds)
	sum.Warnings, sum.AbortReason = res.Warnings, res.AbortReason
	state := "done"
	if res.Aborted {
		state = "aborted"
	}
	span.SetAttributes(attribute.String("tt.loadtest.state", state), attribute.Int64("tt.loadtest.requests", res.Stats.Requests),
		attribute.Int64("tt.loadtest.errors", res.Stats.Errors), attribute.String("tt.loadtest.abort_reason", res.AbortReason))
	log.InfoContext(ctx, "shard finished", "state", state, "requests", res.Stats.Requests, "errors", res.Stats.Errors, "reason", res.AbortReason)
	return finish(state, res.Stats, sum, "", res.AbortReason)
}

// stageSpans records one loadtest.stage span per scheduled stage that started before end (an abort truncates the
// last one). The engine schedules stages from its start, so the spans are reconstructed from the plan.
func stageSpans(ctx context.Context, stages []spec.Stage, start, end time.Time) {
	at := start
	for i, s := range stages {
		if !at.Before(end) {
			return
		}
		stop := at.Add(s.Duration)
		if stop.After(end) {
			stop = end
		}
		_, sp := selftel.StartSpanAt(ctx, "loadtest.stage", at, attribute.Int("tt.loadtest.stage", i),
			attribute.Float64("tt.loadtest.stage_target", s.Target), attribute.Float64("tt.loadtest.stage_duration_s", s.Duration.Seconds()))
		sp.End(trace.WithTimestamp(stop))
		at = at.Add(s.Duration)
	}
}

func runScript(ctx context.Context, plan *spec.Plan, cfg Config, finish func(string, *engine.Stats, any, string, string) error, log *slog.Logger) error {
	timeout := cfg.MaxDuration
	if d := spec.Duration(plan.Stages); d > 0 {
		timeout = d + time.Minute
	}
	res, err := k6.Run(ctx, k6.Options{Script: plan.Script, RunID: fmt.Sprint(cfg.RunID), Timeout: timeout, Binary: cfg.K6Binary, Stdout: os.Stderr})
	if errors.Is(err, k6.ErrNotInstalled) {
		_ = finish("error", nil, nil, "k6 not installed", "")
		return err
	}
	if err != nil {
		_ = finish("error", nil, nil, err.Error(), "")
		return err
	}
	state, reason := "done", ""
	if res.Aborted {
		state, reason = "aborted", "stopped"
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("tt.loadtest.state", state), attribute.Int64("tt.loadtest.requests", res.Summary.Requests))
	log.InfoContext(ctx, "k6 script finished", "state", state, "requests", res.Summary.Requests)
	return finish(state, nil, res.Summary, "", reason)
}
