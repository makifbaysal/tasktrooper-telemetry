// Package engine is the TaskTrooper load engine (ADR-015): a closed model (virtual users following
// stage targets) and an open model (arrival rate with pre-allocated VUs), weighted or sequential steps,
// request templates, checks, thresholds and an automatic abort gate.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tasktrooper/telemetry/services/loadtest/internal/spec"
)

// UserAgent is sent with every request (contracts §14).
const UserAgent = "TaskTrooper-LoadTest/1"

// Options configure one engine run.
type Options struct {
	Plan  *spec.Plan
	RunID string
	// Share scales every stage target (a region's part of the run); 0 = 1.
	Share float64
	// VUCap is a hard cap on concurrent VUs (TT_LOADTEST_MAX_VUS for this shard); 0 = none.
	VUCap int
	// Client overrides the tuned default HTTP client.
	Client *http.Client
	// Tick is the live summary / gate interval (default 5s).
	Tick time.Duration
	// OnTick receives a snapshot every Tick (and once at the end with Final set).
	OnTick func(Tick)
	// GracefulStop lets in-flight iterations finish at the end (default 5s).
	GracefulStop time.Duration
}

// Tick is a periodic snapshot.
type Tick struct {
	Live   Live
	Total  *Stats // cumulative (copy)
	Window *Stats // since the previous tick (copy)
	Final  bool
}

// Result is the outcome of a run.
type Result struct {
	Stats       *Stats
	Aborted     bool
	AbortReason string
	Warnings    []string
}

// Engine runs one plan once.
type Engine struct {
	o      Options
	plan   *spec.Plan
	stages []spec.Stage
	total  time.Duration
	client *http.Client
	cum    []float64 // cumulative step weights
	needs  map[string]bool

	mu       sync.Mutex
	stats    *Stats // cumulative, excluding the open window
	window   *Stats
	warnings map[string]bool

	active  atomic.Int64
	peak    atomic.Int64
	start   time.Time
	abortMu sync.Mutex
	abortCh chan struct{}
	reason  string
}

// New prepares an engine.
func New(o Options) *Engine {
	if o.Share <= 0 {
		o.Share = 1
	}
	if o.Tick <= 0 {
		o.Tick = 5 * time.Second
	}
	if o.GracefulStop <= 0 {
		o.GracefulStop = 5 * time.Second
	}
	p := o.Plan
	e := &Engine{o: o, plan: p, stages: spec.Scale(p.Stages, o.Share), total: spec.Duration(p.Stages),
		stats: NewStats(), window: NewStats(), warnings: map[string]bool{}, abortCh: make(chan struct{})}
	acc := 0.0
	var texts []string
	for _, s := range p.Steps {
		acc += s.Weight
		e.cum = append(e.cum, acc)
		texts = append(texts, s.Body)
		for _, kv := range s.Params {
			texts = append(texts, kv.Value)
		}
		for _, kv := range s.Headers {
			texts = append(texts, kv.Value)
		}
	}
	e.needs = prevRefs(texts...)
	e.client = o.Client
	if e.client == nil {
		e.client = NewClient(e.workers()+16, p.Timeout)
	}
	return e
}

// NewClient returns an HTTP client tuned for load: keep-alive pool sized for the VUs, bounded dial/TLS/header
// timeouts and an overall request timeout.
func NewClient(conns int, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          0,
		MaxIdleConnsPerHost:   conns,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: timeout,
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// workers is the VU pool size: closed model = peak VUs, open model = pre-allocated VUs.
func (e *Engine) workers() int {
	peak := spec.Peak(e.stages)
	var n int
	if e.plan.Mode == "rps" {
		n = int(math.Ceil(float64(e.plan.PeakVUs()) * e.o.Share))
		if e.plan.MaxVUs <= 0 {
			n = max(2*n, 10) // headroom for latency above the 200 ms the estimate assumes
		}
	} else {
		n = int(math.Ceil(peak))
	}
	if e.o.VUCap > 0 {
		n = min(n, e.o.VUCap)
	}
	return max(n, 1)
}

// Abort stops the run early; the first reason wins.
func (e *Engine) Abort(reason string) {
	e.abortMu.Lock()
	defer e.abortMu.Unlock()
	if e.reason != "" {
		return
	}
	e.reason = reason
	close(e.abortCh)
}

func (e *Engine) aborted() string {
	e.abortMu.Lock()
	defer e.abortMu.Unlock()
	return e.reason
}

func (e *Engine) warn(s string) {
	e.mu.Lock()
	if len(e.warnings) < 20 {
		e.warnings[s] = true
	}
	e.mu.Unlock()
}

// Run executes the plan; ctx cancellation aborts with reason "stopped".
func (e *Engine) Run(ctx context.Context) *Result {
	e.start = time.Now()
	// reqCtx outlives the stages by GracefulStop so in-flight requests finish.
	reqCtx, cancelReq := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelReq()
	loadCtx, cancelLoad := context.WithCancel(ctx)
	go func() {
		select {
		case <-e.abortCh:
		case <-ctx.Done():
			e.Abort("stopped")
		case <-loadCtx.Done():
			return
		}
		cancelLoad()
	}()

	var wg sync.WaitGroup
	tickDone := make(chan struct{})
	go e.ticker(loadCtx, tickDone)
	if e.plan.Mode == "rps" {
		e.runOpen(loadCtx, reqCtx, &wg)
	} else {
		e.runClosed(loadCtx, reqCtx, &wg)
	}
	elapsed := time.Since(e.start)
	cancelLoad()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(e.o.GracefulStop):
		cancelReq()
		<-done
	}
	<-tickDone

	e.mu.Lock()
	e.stats.Merge(e.window)
	e.window = NewStats()
	e.stats.ElapsedS = elapsed.Seconds()
	e.stats.PeakVUs = float64(e.peak.Load())
	res := &Result{Stats: e.stats, AbortReason: e.aborted()}
	for w := range e.warnings {
		res.Warnings = append(res.Warnings, w)
	}
	e.mu.Unlock()
	sort.Strings(res.Warnings)
	res.Aborted = res.AbortReason != ""
	if e.o.OnTick != nil {
		e.o.OnTick(Tick{Live: e.live(res.Stats, res.Stats, elapsed, 0), Total: res.Stats, Window: NewStats(), Final: true})
	}
	return res
}

func (e *Engine) live(window, total *Stats, elapsed, span time.Duration) Live {
	l := Live{TS: time.Now().UnixMilli(), Vus: float64(e.active.Load()), Requests: total.Requests,
		P95: round(window.Hist.Quantile(0.95), 2), ErrorRate: round(window.ErrorRate(), 5)}
	if span > 0 {
		l.Rps = round(float64(window.Requests)/span.Seconds(), 2)
	} else if elapsed > 0 {
		l.Rps = round(float64(total.Requests)/elapsed.Seconds(), 2)
	}
	if e.total > 0 {
		l.Progress = round(math.Min(1, float64(elapsed)/float64(e.total)), 4)
	}
	return l
}

// ticker publishes live snapshots and applies the abort gate.
func (e *Engine) ticker(ctx context.Context, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(e.o.Tick)
	defer t.Stop()
	last := e.start
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			e.mu.Lock()
			w := e.window
			e.window = NewStats()
			e.stats.Merge(w)
			e.stats.ElapsedS = now.Sub(e.start).Seconds()
			e.stats.PeakVUs = float64(e.peak.Load())
			total := cloneStats(e.stats)
			e.mu.Unlock()
			span := now.Sub(last)
			last = now
			e.gate(w)
			if e.o.OnTick != nil {
				e.o.OnTick(Tick{Live: e.live(w, total, now.Sub(e.start), span), Total: total, Window: w})
			}
		}
	}
}

func (e *Engine) gate(w *Stats) {
	g := e.plan.Gate
	if w.Requests < int64(max(g.MinRequests, 1)) {
		return
	}
	if g.AbortErrorRate > 0 && w.ErrorRate() > g.AbortErrorRate {
		e.Abort(fmt.Sprintf("gate: error rate %.1f%% > %.1f%%", w.ErrorRate()*100, g.AbortErrorRate*100))
	}
	if g.AbortP95Ms > 0 && w.Hist.Quantile(0.95) > g.AbortP95Ms {
		e.Abort(fmt.Sprintf("gate: p95 %.0f ms > %.0f ms", w.Hist.Quantile(0.95), g.AbortP95Ms))
	}
}

func cloneStats(s *Stats) *Stats {
	c := NewStats()
	c.Merge(s)
	c.ElapsedS, c.PeakVUs = s.ElapsedS, s.PeakVUs
	return c
}

// ---------------------------------------------------------------- closed model

func (e *Engine) runClosed(loadCtx, reqCtx context.Context, wg *sync.WaitGroup) {
	type vu struct{ cancel context.CancelFunc }
	var vus []vu
	limit := e.workers()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	last := time.Now()
	seed := uint64(time.Now().UnixNano())
	for {
		now := time.Now()
		el := now.Sub(e.start)
		e.addVUSeconds(float64(len(vus)) * now.Sub(last).Seconds())
		last = now
		if el >= e.total || loadCtx.Err() != nil {
			break
		}
		want := min(int(math.Round(spec.TargetAt(e.stages, el))), limit)
		for len(vus) < want {
			ctx, cancel := context.WithCancel(loadCtx)
			vus = append(vus, vu{cancel})
			seed++
			wg.Add(1)
			go func(s uint64) {
				defer wg.Done()
				e.active.Add(1)
				e.notePeak()
				defer e.active.Add(-1)
				e.vuLoop(ctx, reqCtx, s)
			}(seed)
		}
		for len(vus) > want {
			vus[len(vus)-1].cancel()
			vus = vus[:len(vus)-1]
		}
		select {
		case <-loadCtx.Done():
		case <-tick.C:
		}
	}
	for _, v := range vus {
		v.cancel()
	}
}

func (e *Engine) notePeak() {
	for {
		a, p := e.active.Load(), e.peak.Load()
		if a <= p || e.peak.CompareAndSwap(p, a) {
			return
		}
	}
}

func (e *Engine) addVUSeconds(v float64) {
	e.mu.Lock()
	e.window.VUSeconds += v
	e.mu.Unlock()
}

func (e *Engine) newVU(seed uint64) *vuState {
	vu := &vuState{rng: mrand.New(mrand.NewPCG(seed, seed*0x9e3779b97f4a7c15+1)), session: map[string]any{}, prev: map[string]any{}}
	if len(e.plan.Session) > 0 {
		r := &renderer{vu: vu, pools: e.plan.Pools, rows: map[string]any{}, warn: e.warn}
		names := make([]string, 0, len(e.plan.Session))
		for k := range e.plan.Session {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			vu.session[k] = r.Value(e.plan.Session[k])
		}
	}
	return vu
}

func (e *Engine) vuLoop(ctx, reqCtx context.Context, seed uint64) {
	vu := e.newVU(seed)
	for ctx.Err() == nil {
		if !e.iteration(ctx, reqCtx, vu, true) {
			return
		}
	}
}

// iteration runs one weighted request or the whole sequential flow. It returns false when ctx ended.
func (e *Engine) iteration(ctx, reqCtx context.Context, vu *vuState, think bool) bool {
	if e.plan.Sequential {
		clear(vu.prev)
		for i := range e.plan.Steps {
			if ctx.Err() != nil {
				return false
			}
			ok := e.do(reqCtx, vu, &e.plan.Steps[i])
			if (think || i < len(e.plan.Steps)-1) && !e.sleep(ctx, e.pause(ok)) {
				return false
			}
		}
		return true
	}
	ok := e.do(reqCtx, vu, e.pick(vu.rng))
	if think {
		return e.sleep(ctx, e.pause(ok))
	}
	return ctx.Err() == nil
}

// pause is the think time; a VU without think time backs off briefly after transport errors so a dead
// target does not spin the CPU.
func (e *Engine) pause(ok bool) time.Duration {
	if e.plan.ThinkTime == 0 && !ok {
		return 10 * time.Millisecond
	}
	return e.plan.ThinkTime
}

func (e *Engine) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (e *Engine) pick(rng *mrand.Rand) *spec.Step {
	x := rng.Float64() * e.cum[len(e.cum)-1]
	for i, c := range e.cum {
		if x < c {
			return &e.plan.Steps[i]
		}
	}
	return &e.plan.Steps[len(e.plan.Steps)-1]
}

// ---------------------------------------------------------------- open model

func (e *Engine) runOpen(loadCtx, reqCtx context.Context, wg *sync.WaitGroup) {
	n := e.workers()
	work := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			vu := e.newVU(seed)
			for {
				select {
				case <-loadCtx.Done():
					return
				case <-work:
				}
				e.active.Add(1)
				e.notePeak()
				e.iteration(loadCtx, reqCtx, vu, false)
				e.active.Add(-1)
			}
		}(uint64(time.Now().UnixNano()) + uint64(i))
	}
	var dispatched int64
	last := time.Now()
	for {
		now := time.Now()
		el := now.Sub(e.start)
		e.addVUSeconds(float64(e.active.Load()) * now.Sub(last).Seconds())
		last = now
		if el >= e.total || loadCtx.Err() != nil {
			return
		}
		due := int64(spec.Integral(e.stages, el))
		var dropped int64
		for dispatched < due {
			select {
			case work <- struct{}{}:
			default:
				dropped++
			}
			dispatched++
		}
		if dropped > 0 {
			e.mu.Lock()
			e.window.Dropped += dropped
			e.mu.Unlock()
		}
		// sleep until the next arrival is due (≤ 10 ms so ramps are followed closely)
		wait := 10 * time.Millisecond
		if r := spec.TargetAt(e.stages, el); r > 0 {
			next := (float64(due+1) - spec.Integral(e.stages, el)) / r
			wait = min(wait, max(time.Duration(next*float64(time.Second)), 200*time.Microsecond))
		}
		select {
		case <-loadCtx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// ---------------------------------------------------------------- requests

type checkResult struct {
	name string
	ok   bool
}

func (e *Engine) buildRequest(ctx context.Context, vu *vuState, st *spec.Step) (*http.Request, error) {
	r := &renderer{vu: vu, pools: e.plan.Pools, rows: map[string]any{}, warn: e.warn}
	path, rawQuery, _ := strings.Cut(st.Path, "?")
	q, _ := url.ParseQuery(rawQuery)
	for _, kv := range st.Params {
		v := r.Value(kv.Value)
		if _, skip := v.(absent); skip {
			continue
		}
		s := toText(v)
		if seg := ":" + kv.Name; strings.Contains(path, seg) {
			path = replaceSegment(path, seg, url.PathEscape(s))
			continue
		}
		if strings.Contains(path, "{"+kv.Name+"}") {
			path = strings.ReplaceAll(path, "{"+kv.Name+"}", url.PathEscape(s))
			continue
		}
		q.Add(kv.Name, s)
	}
	u := *e.plan.Target
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(path, "/")
	u.RawPath = ""
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	var body io.Reader
	isJSON := false
	switch {
	case st.BodyJSON != nil:
		b, err := json.Marshal(r.JSON(st.BodyJSON))
		if err != nil {
			return nil, err
		}
		body, isJSON = bytes.NewReader(b), true
	case strings.TrimSpace(st.Body) != "":
		body = strings.NewReader(r.String(st.Body))
	}
	req, err := http.NewRequestWithContext(ctx, st.Method, u.String(), body)
	if err != nil {
		return nil, err
	}
	for _, kv := range st.Headers {
		v := r.Value(kv.Value)
		if _, skip := v.(absent); skip {
			continue
		}
		req.Header.Set(kv.Name, toText(v))
	}
	if isJSON && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("traceparent", fmt.Sprintf("00-%016x%016x-%016x-01", vu.rng.Uint64(), vu.rng.Uint64()|1, vu.rng.Uint64()|1))
	req.Header.Set("baggage", "tt.svc=loadtest:"+e.o.RunID)
	req.Header.Set("x-trooper-loadtest-run", e.o.RunID)
	req.Header.Set("User-Agent", UserAgent)
	return req, nil
}

// replaceSegment replaces the path parameter seg (":id") only where it is a whole segment.
func replaceSegment(path, seg, val string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == seg {
			parts[i] = val
		}
	}
	return strings.Join(parts, "/")
}

// do performs one request; it returns false on a transport error.
func (e *Engine) do(ctx context.Context, vu *vuState, st *spec.Step) bool {
	req, err := e.buildRequest(ctx, vu, st)
	if err != nil {
		e.warn(fmt.Sprintf("step %s: %v", st.Key, err))
		e.record(st.Key, 0, 0, true, nil)
		return false
	}
	start := time.Now()
	resp, err := e.client.Do(req)
	status := 0
	if err == nil {
		status = resp.StatusCode
		if e.needs[st.Key] {
			var v any
			b, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			if rerr == nil && json.Unmarshal(b, &v) == nil {
				vu.prev[st.Key] = v
			}
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
	}
	ms := float64(time.Since(start).Microseconds()) / 1000
	if err != nil && ctx.Err() != nil {
		return false // cut off at the end of the run: not a target error
	}
	failed := err != nil
	var results []checkResult
	if len(st.Checks) == 0 {
		failed = failed || status >= 400
	} else {
		for _, c := range st.Checks {
			ok := err == nil && c.Pass(status, ms)
			results = append(results, checkResult{c.Name, ok})
			failed = failed || !ok
		}
	}
	e.record(st.Key, status, ms, failed, results)
	return err == nil
}

func (e *Engine) record(step string, status int, ms float64, failed bool, checks []checkResult) {
	e.mu.Lock()
	e.window.record(step, status, ms, failed, checks)
	e.mu.Unlock()
}

// StatusLabel is the status code histogram key.
func StatusLabel(code int) string {
	if code == 0 {
		return "error"
	}
	return strconv.Itoa(code)
}
