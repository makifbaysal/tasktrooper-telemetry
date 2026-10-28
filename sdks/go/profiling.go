package trooper

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Profile types (.ai/contracts.md §24). Go supports all except "wall".
const (
	ProfileCPU          = "cpu"
	ProfileAllocSpace   = "alloc_space"
	ProfileAllocObjects = "alloc_objects"
	ProfileInuseSpace   = "inuse_space"
	ProfileInuseObjects = "inuse_objects"
	ProfileGoroutines   = "goroutines"
	ProfileLock         = "lock"  // mutex contention (enables runtime.SetMutexProfileFraction)
	ProfileBlock        = "block" // blocking on channels/select/sync (enables runtime.SetBlockProfileRate)
)

// DefaultProfileTypes are collected when no types are configured.
var DefaultProfileTypes = []string{ProfileCPU, ProfileAllocSpace, ProfileInuseSpace, ProfileGoroutines}

const (
	// mutexProfileFraction samples 1/N mutex contention events while "lock" profiling is on.
	mutexProfileFraction = 10
	// blockProfileRate samples on average one blocking event per this many nanoseconds blocked.
	blockProfileRate = 100_000
	// maxFoldedBody keeps folded uploads under the ingest limit (4 MB).
	maxFoldedBody = 3 << 20
)

// ProfilingConfig configures continuous profiling (ADR-022). Zero values are filled from the
// environment, then defaults.
type ProfilingConfig struct {
	// Enabled turns continuous profiling on. Env: TROOPER_PROFILING=true.
	Enabled bool
	// Types to collect: cpu, alloc_space, alloc_objects, inuse_space, inuse_objects, goroutines,
	// lock, block. Env: TROOPER_PROFILING_TYPES (comma separated).
	// Default: cpu, alloc_space, inuse_space, goroutines.
	Types []string
	// Period between collections. Env: TROOPER_PROFILING_PERIOD (seconds). Default 60s.
	Period time.Duration
	// CPUDuration is how long the CPU profiler runs in each period (≤ Period).
	// Env: TROOPER_PROFILING_CPU_DURATION (seconds). Default 10s.
	CPUDuration time.Duration
}

// ProfilingOption configures WithProfiling.
type ProfilingOption func(*ProfilingConfig)

// WithProfiling enables continuous profiling: every period a CPU profile of CPUDuration and
// heap/goroutine (and optionally mutex/block) profiles are uploaded to POST /v1/profiles. Requests
// handled by the trooper integrations carry pprof labels trooper.route / trooper.method so flame
// graphs can be filtered per endpoint.
//
//	trooper.Start(ctx, trooper.Config{}, trooper.WithProfiling())
//	trooper.Start(ctx, trooper.Config{}, trooper.WithProfiling(trooper.WithProfileTypes("cpu", "lock")))
func WithProfiling(opts ...ProfilingOption) Option {
	return func(c *Config) {
		c.Profiling.Enabled = true
		for _, o := range opts {
			if o != nil {
				o(&c.Profiling)
			}
		}
	}
}

// WithProfileTypes sets the profile types to collect (see ProfilingConfig.Types).
func WithProfileTypes(types ...string) ProfilingOption {
	return func(c *ProfilingConfig) { c.Types = append([]string(nil), types...) }
}

// WithProfilePeriod sets the collection period (default 60s).
func WithProfilePeriod(d time.Duration) ProfilingOption {
	return func(c *ProfilingConfig) { c.Period = d }
}

// WithProfileCPUDuration sets the CPU profile duration per period (default 10s).
func WithProfileCPUDuration(d time.Duration) ProfilingOption {
	return func(c *ProfilingConfig) { c.CPUDuration = d }
}

func (c ProfilingConfig) withDefaults() ProfilingConfig {
	if !c.Enabled {
		c.Enabled, _ = strconv.ParseBool(strings.TrimSpace(os.Getenv("TROOPER_PROFILING")))
	}
	if len(c.Types) == 0 {
		for _, t := range strings.Split(os.Getenv("TROOPER_PROFILING_TYPES"), ",") {
			if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
				c.Types = append(c.Types, t)
			}
		}
	}
	if len(c.Types) == 0 {
		c.Types = slices.Clone(DefaultProfileTypes)
	}
	if c.Period <= 0 {
		c.Period = envSeconds("TROOPER_PROFILING_PERIOD")
	}
	if c.Period <= 0 {
		c.Period = 60 * time.Second
	}
	if c.CPUDuration <= 0 {
		c.CPUDuration = envSeconds("TROOPER_PROFILING_CPU_DURATION")
	}
	if c.CPUDuration <= 0 {
		c.CPUDuration = 10 * time.Second
	}
	if c.CPUDuration > c.Period {
		c.CPUDuration = c.Period
	}
	return c
}

func envSeconds(name string) time.Duration {
	f, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64)
	if err != nil || f <= 0 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

// profiler collects and uploads profiles in a background goroutine.
type profiler struct {
	cfg      ProfilingConfig
	client   *http.Client
	url      string // endpoint + /v1/profiles
	key      string
	base     url.Values
	timeout  time.Duration
	debug    bool
	cpu      bool
	heap     map[string]bool // alloc_space, alloc_objects, inuse_space, inuse_objects
	goroutin bool
	mutex    bool
	block    bool

	prevMutexFraction int
	// prev holds the previous cumulative values per type and folded stack (alloc_*, lock, block).
	prev        map[string]map[string]int64
	lastCollect time.Time

	backoff      time.Duration
	backoffUntil time.Time

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func startProfiler(cfg Config, client *http.Client, endpoint, key string) (p *profiler) {
	defer func() {
		if recover() != nil {
			self.countError("profiler")
			p = nil
		}
	}()
	if endpoint == "" {
		return nil
	}
	p = &profiler{
		cfg:     cfg.Profiling,
		client:  client,
		url:     endpoint + "/v1/profiles",
		key:     key,
		timeout: min(cfg.ExportTimeout, 30*time.Second),
		debug:   cfg.Debug,
		heap:    map[string]bool{},
		prev:    map[string]map[string]int64{},
		done:    make(chan struct{}),
	}
	p.base = url.Values{"service": {cfg.Service}, "runtime": {"go"}}
	if cfg.Environment != "" {
		p.base.Set("env", cfg.Environment)
	}
	if cfg.Version != "" {
		p.base.Set("version", cfg.Version)
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		p.base.Set("host", h)
	}
	for _, t := range p.cfg.Types {
		switch t {
		case ProfileCPU:
			p.cpu = true
		case ProfileAllocSpace, ProfileAllocObjects, ProfileInuseSpace, ProfileInuseObjects:
			p.heap[t] = true
		case ProfileGoroutines, "goroutine":
			p.goroutin = true
		case ProfileLock, "mutex":
			p.mutex = true
		case ProfileBlock:
			p.block = true
		default:
			p.logf("unsupported profile type %q ignored", t)
		}
	}
	if !p.cpu && len(p.heap) == 0 && !p.goroutin && !p.mutex && !p.block {
		return nil
	}
	if p.mutex {
		if cur := runtime.SetMutexProfileFraction(-1); cur == 0 {
			p.prevMutexFraction = runtime.SetMutexProfileFraction(mutexProfileFraction)
		} else {
			p.prevMutexFraction = -1 // the application set its own fraction: keep it
		}
	}
	if p.block {
		runtime.SetBlockProfileRate(blockProfileRate)
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.lastCollect = time.Now()
	profilerLabels.Store(true)
	go p.run()
	return p
}

func (p *profiler) logf(format string, args ...any) {
	if p.debug {
		fmt.Fprintf(os.Stderr, "trooper: profiler: "+format+"\n", args...)
	}
}

// shutdown stops the loop (an in-flight CPU profile is stopped and discarded) and waits for it.
func (p *profiler) shutdown(ctx context.Context) error {
	p.once.Do(func() {
		p.cancel()
		select {
		case <-p.done:
		case <-ctx.Done():
		}
		profilerLabels.Store(false)
		if p.mutex && p.prevMutexFraction >= 0 {
			runtime.SetMutexProfileFraction(p.prevMutexFraction)
		}
		if p.block {
			runtime.SetBlockProfileRate(0)
		}
	})
	return nil
}

func (p *profiler) run() {
	defer close(p.done)
	defer func() {
		if recover() != nil {
			self.countError("profiler")
		}
	}()
	for {
		start := time.Now()
		if !p.cycle() {
			return
		}
		if !p.sleep(p.cfg.Period - time.Since(start)) {
			return
		}
	}
}

// sleep waits d; false if the profiler was stopped.
func (p *profiler) sleep(d time.Duration) bool {
	if d <= 0 {
		return p.ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-p.ctx.Done():
		return false
	}
}

// cycle collects one round of profiles. It returns false when the profiler was stopped.
func (p *profiler) cycle() bool {
	if p.cpu {
		var buf bytes.Buffer
		start := time.Now()
		if err := pprof.StartCPUProfile(&buf); err != nil {
			// The application (or net/http/pprof) is already profiling: skip this round.
			self.countError("profiler.cpu")
			p.logf("cpu profile skipped: %v", err)
			if !p.sleep(p.cfg.CPUDuration) {
				return false
			}
		} else {
			ok := p.sleep(p.cfg.CPUDuration)
			pprof.StopCPUProfile()
			if !ok {
				return false
			}
			p.upload(buf.Bytes(), start, time.Now(), "pprof", "", "")
		}
	}
	if p.ctx.Err() != nil {
		return false
	}
	now := time.Now()
	from := p.lastCollect
	p.lastCollect = now
	if len(p.heap) > 0 {
		p.collectFolded("heap", from, now, map[string]typeSpec{
			ProfileAllocSpace:   {"alloc_space/bytes", "bytes", true},
			ProfileAllocObjects: {"alloc_objects/count", "count", true},
			ProfileInuseSpace:   {"inuse_space/bytes", "bytes", false},
			ProfileInuseObjects: {"inuse_objects/count", "count", false},
		})
	}
	if p.goroutin {
		var buf bytes.Buffer
		if err := pprof.Lookup("goroutine").WriteTo(&buf, 0); err == nil {
			p.upload(buf.Bytes(), from, now, "pprof", "", "")
		} else {
			self.countError("profiler")
		}
	}
	if p.mutex {
		p.collectFolded("mutex", from, now, map[string]typeSpec{ProfileLock: {"delay/nanoseconds", "nanoseconds", true}})
	}
	if p.block {
		p.collectFolded("block", from, now, map[string]typeSpec{ProfileBlock: {"delay/nanoseconds", "nanoseconds", true}})
	}
	return p.ctx.Err() == nil
}

type typeSpec struct {
	sampleType string // pprof "type/unit"
	unit       string
	cumulative bool // value is cumulative since process start: upload the delta
}

// collectFolded reads a runtime profile, and uploads one folded profile per requested type.
// Cumulative types (allocations, mutex, block) are converted to the delta since the previous
// collection; the first collection only records the baseline.
func (p *profiler) collectFolded(name string, from, to time.Time, specs map[string]typeSpec) {
	var buf bytes.Buffer
	if err := pprof.Lookup(name).WriteTo(&buf, 0); err != nil {
		self.countError("profiler")
		return
	}
	prof, err := decodePprof(buf.Bytes())
	if err != nil {
		self.countError("profiler")
		p.logf("decode %s profile: %v", name, err)
		return
	}
	for typ, spec := range specs {
		if name == "heap" && !p.heap[typ] {
			continue
		}
		idx := prof.sampleIndex(spec.sampleType)
		if idx < 0 {
			continue
		}
		cur := make(map[string]int64, len(prof.samples))
		for _, s := range prof.samples {
			if idx < len(s.values) && s.values[idx] != 0 {
				cur[foldedKey(s.stack)] += s.values[idx]
			}
		}
		vals := cur
		if spec.cumulative {
			prev, seen := p.prev[typ]
			p.prev[typ] = cur
			if !seen {
				continue // baseline only
			}
			vals = make(map[string]int64, len(cur))
			for k, v := range cur {
				if d := v - prev[k]; d > 0 {
					vals[k] = d
				}
			}
		}
		if body := foldedBody(vals); len(body) > 0 {
			p.upload(body, from, to, "folded", typ, spec.unit)
		}
	}
}

// foldedBody renders stacks as "root;...;leaf value" lines, largest first; stacks beyond the size
// limit are merged into a single "[other]" stack.
func foldedBody(vals map[string]int64) []byte {
	type kv struct {
		k string
		v int64
	}
	list := make([]kv, 0, len(vals))
	for k, v := range vals {
		if v > 0 {
			list = append(list, kv{k, v})
		}
	}
	if len(list) == 0 {
		return nil
	}
	slices.SortFunc(list, func(a, b kv) int {
		if a.v != b.v {
			if a.v > b.v {
				return -1
			}
			return 1
		}
		return strings.Compare(a.k, b.k)
	})
	var b bytes.Buffer
	var other int64
	for _, e := range list {
		if b.Len()+len(e.k)+24 > maxFoldedBody {
			other += e.v
			continue
		}
		b.WriteString(e.k)
		b.WriteByte(' ')
		b.WriteString(strconv.FormatInt(e.v, 10))
		b.WriteByte('\n')
	}
	if other > 0 {
		b.WriteString("[other] " + strconv.FormatInt(other, 10) + "\n")
	}
	return b.Bytes()
}

// upload sends one profile. Failed uploads are dropped (no queue); 429/5xx/network errors start an
// exponential backoff (1s → 60s, jitter, Retry-After honoured) during which profiles are dropped.
func (p *profiler) upload(body []byte, start, end time.Time, format, typ, unit string) {
	defer func() {
		if recover() != nil {
			self.countError("profiler")
		}
	}()
	if len(body) == 0 {
		return
	}
	if time.Now().Before(p.backoffUntil) {
		self.countDropped("profiler", 1)
		return
	}
	q := url.Values{}
	for k, v := range p.base {
		q[k] = v
	}
	q.Set("start", strconv.FormatInt(start.UnixMilli(), 10))
	q.Set("end", strconv.FormatInt(max(end.UnixMilli(), start.UnixMilli()+1), 10))
	q.Set("format", format)
	contentType := "application/octet-stream"
	if typ != "" {
		q.Set("type", typ)
		q.Set("unit", unit)
		contentType = "text/plain; charset=utf-8"
	}
	ctx, cancel := context.WithTimeout(p.ctx, p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+"?"+q.Encode(), bytes.NewReader(body))
	if err != nil {
		self.countError("profiler")
		return
	}
	req.Header.Set("Content-Type", contentType)
	if p.key != "" {
		req.Header.Set("x-trooper-key", p.key)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if p.ctx.Err() == nil {
			p.fail("profiler", 0, "")
			p.logf("upload: %v", err)
		}
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		p.backoff = 0
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		p.fail("profiler", resp.StatusCode, resp.Header.Get("Retry-After"))
	default: // other 4xx: drop
		self.countDropped("profiler", 1)
		self.countError("exporter.profiles")
		p.logf("upload rejected: %s", resp.Status)
	}
}

func (p *profiler) fail(component string, status int, retryAfter string) {
	self.countDropped(component, 1)
	self.countError("exporter.profiles")
	if p.backoff <= 0 {
		p.backoff = time.Second
	} else {
		p.backoff = min(p.backoff*2, 60*time.Second)
	}
	wait := p.backoff/2 + time.Duration(rand.Int64N(int64(p.backoff/2)+1))
	if secs, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && secs > 0 {
		wait = max(wait, min(time.Duration(secs)*time.Second, 5*time.Minute))
	}
	p.backoffUntil = time.Now().Add(wait)
	if status != 0 {
		p.logf("upload failed with %d; backing off %s", status, wait)
	}
}
