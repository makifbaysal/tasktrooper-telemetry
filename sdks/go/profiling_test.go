package trooper

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

type profileReq struct {
	query       url.Values
	key         string
	contentType string
	body        []byte
}

type profileSink struct {
	mu     sync.Mutex
	reqs   []profileReq
	status int
}

func (s *profileSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/profiles" {
		w.WriteHeader(http.StatusOK)
		return
	}
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.reqs = append(s.reqs, profileReq{r.URL.Query(), r.Header.Get("x-trooper-key"), r.Header.Get("Content-Type"), b})
	status := s.status
	s.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
}

func (s *profileSink) byType() map[string][]profileReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]profileReq{}
	for _, r := range s.reqs {
		t := r.query.Get("type")
		if t == "" { // pprof: identify by sample types
			if p, err := decodePprof(r.body); err == nil {
				switch {
				case p.sampleIndex("cpu/nanoseconds") >= 0:
					t = "cpu"
				case p.sampleIndex("goroutine/count") >= 0:
					t = "goroutines"
				}
			}
		}
		out[t] = append(out[t], r)
	}
	return out
}

func (s *profileSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

var foldedLine = regexp.MustCompile(`^[^;\n]+(;[^;\n]+)* [0-9]+$`)

func burnCPU(stop <-chan struct{}) {
	x := 0
	for {
		select {
		case <-stop:
			return
		default:
		}
		for i := 0; i < 100000; i++ {
			x += i * i
		}
		b := make([]byte, 128<<10+x%2) // non-constant size: heap allocation for alloc_space
		x += int(b[0])
	}
}

func TestProfilingUpload(t *testing.T) {
	sink := &profileSink{}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	prevTP, prevMP := otel.GetTracerProvider(), otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prevTP); otel.SetMeterProvider(prevMP) })

	shutdown, err := Start(context.Background(), Config{
		DSN:            strings.Replace(srv.URL, "http://", "http://tt_prof_key@", 1) + "/1",
		Service:        "orders",
		Environment:    "test",
		Version:        "1.2.3",
		DisableTraces:  true,
		DisableMetrics: true,
		DisableLogs:    true,
	}, WithProfiling(
		WithProfileTypes("cpu", "alloc_space", "inuse_space", "goroutines", "lock", "block"),
		WithProfilePeriod(400*time.Millisecond),
		WithProfileCPUDuration(150*time.Millisecond),
	))
	if err != nil {
		t.Fatal(err)
	}
	if !ProfileLabelsEnabled() {
		t.Error("profile labels not enabled while profiling")
	}

	stop := make(chan struct{})
	go burnCPU(stop)
	// Label a busy goroutine so the CPU profile carries trooper.route.
	go pprof.Do(context.Background(), pprof.Labels(ProfileLabelRoute, "/busy/:id", ProfileLabelMethod, "GET"), func(context.Context) { burnCPU(stop) })

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		bt := sink.byType()
		if len(bt["cpu"]) >= 1 && len(bt["goroutines"]) >= 1 && len(bt["inuse_space"]) >= 1 && len(bt["alloc_space"]) >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(stop)
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ProfileLabelsEnabled() {
		t.Error("profile labels still enabled after shutdown")
	}
	n := sink.count()
	time.Sleep(600 * time.Millisecond)
	if sink.count() != n {
		t.Error("profiles uploaded after shutdown")
	}

	bt := sink.byType()
	for _, typ := range []string{"cpu", "goroutines", "inuse_space", "alloc_space"} {
		if len(bt[typ]) == 0 {
			t.Fatalf("no %s profile uploaded; got %v", typ, keys(bt))
		}
	}
	for typ, reqs := range bt {
		for _, r := range reqs {
			q := r.query
			for k, want := range map[string]string{"service": "orders", "env": "test", "version": "1.2.3", "runtime": "go"} {
				if q.Get(k) != want {
					t.Errorf("%s: %s=%q want %q", typ, k, q.Get(k), want)
				}
			}
			if q.Get("host") == "" {
				t.Errorf("%s: host missing", typ)
			}
			start, err1 := strconv.ParseInt(q.Get("start"), 10, 64)
			end, err2 := strconv.ParseInt(q.Get("end"), 10, 64)
			if err1 != nil || err2 != nil || end <= start || start < time.Now().Add(-time.Minute).UnixMilli() {
				t.Errorf("%s: bad window start=%s end=%s", typ, q.Get("start"), q.Get("end"))
			}
			if r.key != "tt_prof_key" {
				t.Errorf("%s: key %q", typ, r.key)
			}
		}
	}

	// pprof uploads: gzip magic, decodable, format=pprof.
	cpu := bt["cpu"][0]
	if cpu.query.Get("format") != "pprof" || cpu.contentType != "application/octet-stream" {
		t.Errorf("cpu: format=%q content-type=%q", cpu.query.Get("format"), cpu.contentType)
	}
	if !bytes.HasPrefix(cpu.body, []byte{0x1f, 0x8b}) {
		t.Fatal("cpu profile is not gzipped")
	}
	if start, _ := strconv.ParseInt(cpu.query.Get("start"), 10, 64); true {
		end, _ := strconv.ParseInt(cpu.query.Get("end"), 10, 64)
		if d := end - start; d < 140 || d > 2000 {
			t.Errorf("cpu window %dms, want ~150ms", d)
		}
	}
	labelled := false
	for _, r := range bt["cpu"] {
		p, err := decodePprof(r.body)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range p.samples {
			if s.labels[ProfileLabelRoute] == "/busy/:id" && s.labels[ProfileLabelMethod] == "GET" {
				labelled = true
			}
		}
	}
	if !labelled {
		t.Error("no CPU sample carries the trooper.route/trooper.method labels")
	}
	g, err := decodePprof(bt["goroutines"][0].body)
	if err != nil || len(g.samples) == 0 {
		t.Fatalf("goroutine profile: %v", err)
	}

	// folded uploads.
	for _, typ := range []string{"inuse_space", "alloc_space"} {
		r := bt[typ][0]
		if r.query.Get("format") != "folded" || r.query.Get("unit") != "bytes" || !strings.HasPrefix(r.contentType, "text/plain") {
			t.Errorf("%s: format=%q unit=%q ct=%q", typ, r.query.Get("format"), r.query.Get("unit"), r.contentType)
		}
		lines := strings.Split(strings.TrimSpace(string(r.body)), "\n")
		for _, l := range lines {
			if !foldedLine.MatchString(l) {
				t.Fatalf("%s: bad folded line %q", typ, l)
			}
		}
	}
	if !strings.Contains(string(bt["alloc_space"][0].body), "burnCPU") {
		t.Error("alloc_space delta does not contain the allocating function")
	}
}

func keys(m map[string][]profileReq) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func newTestProfiler(t *testing.T, srv *httptest.Server) *profiler {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &profiler{
		client: srv.Client(), url: srv.URL + "/v1/profiles", key: "k",
		base: url.Values{"service": {"svc"}, "runtime": {"go"}}, timeout: 5 * time.Second,
		ctx: ctx, cancel: cancel, prev: map[string]map[string]int64{}, heap: map[string]bool{},
	}
}

func TestProfilerBackoff(t *testing.T) {
	tests := []struct {
		status      int
		wantBackoff bool
	}{
		{http.StatusServiceUnavailable, true},
		{http.StatusInternalServerError, true},
		{http.StatusTooManyRequests, true},
		{http.StatusBadRequest, false},
		{http.StatusOK, false},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.status), func(t *testing.T) {
			sink := &profileSink{status: tt.status}
			srv := httptest.NewServer(sink)
			defer srv.Close()
			p := newTestProfiler(t, srv)
			now := time.Now()
			p.upload([]byte("a;b 1\n"), now.Add(-time.Second), now, "folded", "lock", "nanoseconds")
			p.upload([]byte("a;b 1\n"), now.Add(-time.Second), now, "folded", "lock", "nanoseconds")
			want := 2
			if tt.wantBackoff {
				want = 1 // the second profile is dropped during backoff
			}
			if got := sink.count(); got != want {
				t.Errorf("sent %d, want %d", got, want)
			}
			if tt.wantBackoff != p.backoffUntil.After(time.Now()) {
				t.Errorf("backoff active = %v, want %v", p.backoffUntil.After(time.Now()), tt.wantBackoff)
			}
		})
	}
}

func TestProfilerRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	p := newTestProfiler(t, srv)
	p.upload([]byte("x 1\n"), time.Now(), time.Now(), "folded", "lock", "nanoseconds")
	if until := time.Until(p.backoffUntil); until < 25*time.Second {
		t.Errorf("Retry-After not honoured: backoff %v", until)
	}
}

func TestProfilerSkipsWhenCPUProfileActive(t *testing.T) {
	var own bytes.Buffer
	if err := pprof.StartCPUProfile(&own); err != nil {
		t.Skip("cpu profiling already active:", err)
	}
	defer pprof.StopCPUProfile()
	sink := &profileSink{}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	p := newTestProfiler(t, srv)
	p.cpu = true
	p.cfg.CPUDuration = 10 * time.Millisecond
	before := SDKErrors()
	if !p.cycle() {
		t.Fatal("cycle stopped")
	}
	if sink.count() != 0 {
		t.Error("uploaded a CPU profile although the application was profiling")
	}
	if SDKErrors() <= before {
		t.Error("skip not counted in trooper.sdk.errors")
	}
}

func TestProfilingConfigEnv(t *testing.T) {
	t.Setenv("TROOPER_PROFILING", "true")
	t.Setenv("TROOPER_PROFILING_TYPES", "cpu, LOCK")
	t.Setenv("TROOPER_PROFILING_PERIOD", "30")
	t.Setenv("TROOPER_PROFILING_CPU_DURATION", "45")
	c := ProfilingConfig{}.withDefaults()
	if !c.Enabled || len(c.Types) != 2 || c.Types[1] != "lock" || c.Period != 30*time.Second || c.CPUDuration != 30*time.Second {
		t.Errorf("config = %+v", c)
	}
	t.Setenv("TROOPER_PROFILING", "")
	t.Setenv("TROOPER_PROFILING_TYPES", "")
	t.Setenv("TROOPER_PROFILING_PERIOD", "")
	t.Setenv("TROOPER_PROFILING_CPU_DURATION", "")
	c = ProfilingConfig{}.withDefaults()
	if c.Enabled || c.Period != time.Minute || c.CPUDuration != 10*time.Second || len(c.Types) != len(DefaultProfileTypes) {
		t.Errorf("defaults = %+v", c)
	}
	var cfg Config
	WithProfiling(WithProfilePeriod(time.Second))(&cfg)
	if !cfg.Profiling.Enabled || cfg.Profiling.Period != time.Second {
		t.Errorf("WithProfiling = %+v", cfg.Profiling)
	}
}

func TestFoldedBodyOther(t *testing.T) {
	vals := map[string]int64{}
	frame := strings.Repeat("f", 1000)
	for i := 0; i < 5000; i++ {
		vals[frame+";"+strconv.Itoa(i)] = int64(i + 1)
	}
	b := foldedBody(vals)
	if len(b) > maxFoldedBody+64 {
		t.Fatalf("body %d bytes", len(b))
	}
	if !bytes.Contains(b, []byte("\n[other] ")) {
		t.Error("no [other] stack")
	}
	if !bytes.HasPrefix(b, []byte(frame+";4999 5000\n")) {
		t.Error("largest stack not first")
	}
}
