// Package k6 runs raw k6 scripts (scenario spec.script) with the k6 binary on PATH and reads its
// --summary-export JSON (ADR-015: k6 is not embedded).
package k6

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/tasktrooper/telemetry/services/loadtest/internal/engine"
)

// ErrNotInstalled is returned when k6 is not on PATH.
var ErrNotInstalled = errors.New("k6 not installed")

// Options for one script run.
type Options struct {
	Script  string
	RunID   string
	Env     []string // extra KEY=VALUE for the script (__ENV)
	Timeout time.Duration
	Binary  string // default "k6" on PATH
	Stdout  io.Writer
}

// Result of a k6 run.
type Result struct {
	Summary engine.Summary
	Passed  bool
	Aborted bool
}

// Run executes the script. Context cancellation interrupts k6 (SIGINT; it still writes the summary).
func Run(ctx context.Context, o Options) (*Result, error) {
	bin := o.Binary
	if bin == "" {
		bin = "k6"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, ErrNotInstalled
	}
	dir, err := os.MkdirTemp("", "tt-k6-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	script, summary := filepath.Join(dir, "script.js"), filepath.Join(dir, "summary.json")
	if err := os.WriteFile(script, []byte(o.Script), 0o600); err != nil {
		return nil, err
	}
	args := []string{"run", "--quiet", "--no-color", "--summary-export", summary,
		"-e", "TT_LOADTEST_RUN=" + o.RunID, "--tag", "trooper_loadtest_run=" + o.RunID}
	for _, e := range o.Env {
		args = append(args, "-e", e)
	}
	args = append(args, script)
	runCtx := ctx
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	cmd := exec.Command(path, args...)
	cmd.Env = append(os.Environ(), "K6_NO_USAGE_REPORT=true")
	if o.Stdout != nil {
		cmd.Stdout, cmd.Stderr = o.Stdout, o.Stdout
	}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	interrupted := false
	var werr error
	select {
	case werr = <-done:
	case <-runCtx.Done():
		interrupted = true
		_ = cmd.Process.Signal(syscall.SIGINT)
		select {
		case werr = <-done:
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
			werr = <-done
		}
	}
	code := 0
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		code = ee.ExitCode()
	} else if werr != nil {
		return nil, werr
	}
	raw, rerr := os.ReadFile(summary)
	if rerr != nil {
		return nil, fmt.Errorf("k6 exited %d without a summary", code)
	}
	sum, failedThresholds, err := ParseSummary(raw, time.Since(start))
	if err != nil {
		return nil, err
	}
	res := &Result{Summary: sum, Aborted: interrupted}
	switch {
	case interrupted:
	case code == 0:
		res.Passed = !failedThresholds
	case code == 99: // thresholds crossed
	default:
		return nil, fmt.Errorf("k6 exited with code %d", code)
	}
	return res, nil
}

type metric map[string]any

func (m metric) num(k string) float64 {
	if v, ok := m[k].(float64); ok {
		return v
	}
	return 0
}

// ParseSummary converts a k6 --summary-export document into our summary. In that format a threshold value
// of true means the threshold failed.
func ParseSummary(raw []byte, elapsed time.Duration) (engine.Summary, bool, error) {
	var doc struct {
		Metrics map[string]metric `json:"metrics"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return engine.Summary{}, false, fmt.Errorf("k6 summary: %w", err)
	}
	m := func(n string) metric {
		if v := doc.Metrics[n]; v != nil {
			return v
		}
		return metric{}
	}
	dur, reqs, failed := m("http_req_duration"), m("http_reqs"), m("http_req_failed")
	errRate := failed.num("value")
	if errRate == 0 && failed.num("fails")+failed.num("passes") > 0 {
		// legacy rate metric: passes = failed requests
		errRate = failed.num("passes") / (failed.num("fails") + failed.num("passes"))
	}
	count := int64(reqs.num("count"))
	s := engine.Summary{
		Progress: 1, Rps: reqs.num("rate"), P95: dur.num("p(95)"), ErrorRate: errRate, Vus: m("vus_max").num("value"),
		P50: dur.num("med"), P99: dur.num("p(99)"), AvgMs: dur.num("avg"), MaxMs: dur.num("max"), Requests: count,
		Errors: int64(errRate*float64(count) + 0.5), DurationS: elapsed.Seconds(), StatusCodes: map[string]int64{}, Steps: []engine.StepSummary{},
		Thresholds: []engine.ThresholdResult{},
	}
	anyFailed := false
	names := make([]string, 0, len(doc.Metrics))
	for n := range doc.Metrics {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		th, ok := doc.Metrics[n]["thresholds"].(map[string]any)
		if !ok {
			continue
		}
		exprs := make([]string, 0, len(th))
		for e := range th {
			exprs = append(exprs, e)
		}
		sort.Strings(exprs)
		for _, e := range exprs {
			bad, _ := th[e].(bool)
			anyFailed = anyFailed || bad
			s.Thresholds = append(s.Thresholds, engine.ThresholdResult{Name: n + " " + strings.ReplaceAll(e, " ", ""), Passed: !bad})
		}
	}
	return s, anyFailed, nil
}
