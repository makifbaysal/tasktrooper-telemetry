package ctl

import (
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/tasktrooper/telemetry/services/loadtest/internal/engine"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/spec"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/store"
)

// Running is the in-progress view of a run: loadtest_runs.live plus the summary fields the api reads.
type Running struct {
	Live    engine.Live
	Summary map[string]any
}

// MergeLive combines the shards' live snapshots and cumulative stats.
func MergeLive(shards []store.Shard, plan *spec.Plan) Running {
	var l engine.Live
	l.Progress = 1
	total := engine.NewStats()
	var errW, rpsSum float64
	seen := false
	for _, sh := range shards {
		var sl engine.Live
		if len(sh.Live) > 0 && json.Unmarshal(sh.Live, &sl) == nil {
			seen = true
			l.TS = max(l.TS, sl.TS)
			l.Vus += sl.Vus
			l.Rps += sl.Rps
			l.P95 = math.Max(l.P95, sl.P95) // percentiles do not add up across shards: report the worst region
			l.Requests += sl.Requests
			errW += sl.ErrorRate * sl.Rps
			rpsSum += sl.Rps
			l.Progress = math.Min(l.Progress, sl.Progress)
		} else {
			l.Progress = 0
		}
		var st engine.Stats
		if len(sh.Stats) > 0 && json.Unmarshal(sh.Stats, &st) == nil {
			total.Merge(&st)
		}
	}
	if !seen {
		l.Progress = 0
		l.TS = time.Now().UnixMilli()
	}
	if rpsSum > 0 {
		l.ErrorRate = math.Round(errW/rpsSum*1e5) / 1e5
	}
	l.Rps = math.Round(l.Rps*100) / 100
	var ths []engine.ThresholdResult
	if plan != nil {
		ths, _ = engine.Evaluate(plan.Thresholds, total)
	}
	if ths == nil {
		ths = []engine.ThresholdResult{}
	}
	return Running{Live: l, Summary: map[string]any{
		"progress": l.Progress, "rps": l.Rps, "p95": l.P95, "errorRate": l.ErrorRate, "vus": l.Vus,
		"thresholds": ths, "requests": total.Requests, "errors": total.Errors,
	}}
}

// Final is the outcome of a finished run.
type Final struct {
	State     string // passed | failed | aborted | error
	Summary   engine.Summary
	Passed    bool
	Error     string
	VUMinutes float64
}

// MergeFinal combines terminal shards into the run's final summary and state. userAbort is true when an
// abort was requested from outside (api stop) — the run is then 'aborted' regardless of thresholds.
func MergeFinal(shards []store.Shard, plan *spec.Plan, userAbort bool) Final {
	total := engine.NewStats()
	var regions []engine.RegionSummary
	var warnings []string
	seenW := map[string]bool{}
	var errMsg, abortReason string
	var scriptSum *engine.Summary
	for _, sh := range shards {
		var st engine.Stats
		hasStats := len(sh.Stats) > 0 && json.Unmarshal(sh.Stats, &st) == nil
		if hasStats {
			total.Merge(&st)
		}
		var ss engine.Summary
		hasSum := len(sh.Summary) > 0 && json.Unmarshal(sh.Summary, &ss) == nil
		if hasSum && !hasStats {
			c := ss
			scriptSum = &c
		}
		for _, w := range ss.Warnings {
			if !seenW[w] {
				seenW[w] = true
				warnings = append(warnings, w)
			}
		}
		rs := engine.RegionSummary{Region: sh.Region, State: sh.State, Error: sh.Error}
		if hasStats {
			rs.Requests, rs.ErrorRate, rs.P95 = st.Requests, math.Round(st.ErrorRate()*1e5)/1e5, math.Round(st.Hist.Quantile(0.95)*100)/100
		} else if hasSum {
			rs.Requests, rs.ErrorRate, rs.P95 = ss.Requests, ss.ErrorRate, ss.P95
		}
		regions = append(regions, rs)
		if sh.State == "error" && errMsg == "" {
			errMsg = sh.Region + ": " + sh.Error
			if len(shards) == 1 {
				errMsg = sh.Error
			}
		}
		if sh.State == "aborted" && abortReason == "" {
			abortReason = sh.AbortReason
		}
	}
	var ths []spec.Threshold
	if plan != nil {
		ths = plan.Thresholds
	}
	var f Final
	if scriptSum != nil && total.Requests == 0 {
		f.Summary = *scriptSum
		f.Passed = true
		for _, t := range f.Summary.Thresholds {
			f.Passed = f.Passed && t.Passed
		}
	} else {
		f.Summary, f.Passed = engine.Summarize(total, ths)
		f.Summary.Warnings = warnings
	}
	sort.Slice(regions, func(i, j int) bool { return regions[i].Region < regions[j].Region })
	if len(regions) > 1 {
		f.Summary.Regions = regions
	}
	f.VUMinutes = math.Round(total.VUSeconds/60*1000) / 1000
	f.Summary.VUMinutes = f.VUMinutes
	switch {
	case errMsg != "":
		f.State, f.Error, f.Passed = "error", errMsg, false
	case userAbort:
		f.State = "aborted"
		if abortReason == "" || abortReason == "stopped" {
			abortReason = "stopped by user"
		}
	case abortReason != "":
		f.State = "aborted"
	case f.Passed:
		f.State = "passed"
	default:
		f.State = "failed"
	}
	if f.State == "aborted" {
		f.Summary.AbortReason = abortReason
		if plan != nil && spec.Duration(plan.Stages) > 0 && total.ElapsedS > 0 {
			f.Summary.Progress = math.Round(math.Min(1, total.ElapsedS/spec.Duration(plan.Stages).Seconds())*1e4) / 1e4
		}
	}
	if f.Summary.Thresholds == nil {
		f.Summary.Thresholds = []engine.ThresholdResult{}
	}
	return f
}
