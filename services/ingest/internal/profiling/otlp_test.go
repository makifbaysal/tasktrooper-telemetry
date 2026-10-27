package profiling

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"slices"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/ingest/internal/profiling/otlptest"
)

const t0 = uint64(1_730_000_000_000_000_000) // ns

var goRes = []string{"service.name", "api", "deployment.environment.name", "prod", "service.version", "1.2.0", "host.name", "h1",
	"process.runtime.name", "go"}

func stackOf(p OTLPProfile, frames ...string) (Stack, bool) {
	for _, s := range p.Series.Stacks {
		if slices.Equal(s.Frames, frames) {
			return s, true
		}
	}
	return Stack{}, false
}

func TestConvertOTLP(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	tests := []struct {
		name  string
		build func(b *otlptest.Builder)
		check func(t *testing.T, r OTLPResult)
	}{
		{"cpu nanoseconds with route attributes", func(b *otlptest.Builder) {
			h := b.Stack(b.Loc("main.handler"), b.Loc("main.main"))
			gc := b.Stack(b.Loc("runtime.gcBgMarkWorker"), b.Loc("main.main"))
			route, method := b.Attr(LabelRoute, "/users/:id"), b.Attr(LabelMethod, "get")
			b.Resource(goRes, &profilespb.Profile{SampleType: b.VT("cpu", "nanoseconds"), TimeUnixNano: t0, DurationNano: uint64(10 * time.Second),
				PeriodType: b.VT("cpu", "nanoseconds"), Period: 10_000_000,
				Samples: []*profilespb.Sample{
					{StackIndex: h, Values: []int64{30_000_000}, AttributeIndices: []int32{route, method}},
					{StackIndex: h, Values: []int64{10_000_000}, AttributeIndices: []int32{method, route}},
					{StackIndex: gc, Values: []int64{5_000_000}},
					{StackIndex: gc, Values: []int64{0}},
				}})
		}, func(t *testing.T, r OTLPResult) {
			if len(r.Profiles) != 1 || r.NoService != 0 {
				t.Fatalf("result %+v", r)
			}
			p := r.Profiles[0]
			if p.Service != "api" || p.Env != "prod" || p.Version != "1.2.0" || p.Host != "h1" || p.Runtime != "go" ||
				p.Start.UnixNano() != int64(t0) || p.End.Sub(p.Start) != 10*time.Second {
				t.Fatalf("identity %+v", p)
			}
			if p.Series.Type != "cpu" || p.Series.Unit != "nanoseconds" || p.Series.Total != 45_000_000 || len(p.Series.Stacks) != 2 {
				t.Fatalf("series %+v", p.Series)
			}
			s, ok := stackOf(p, "main.main", "main.handler")
			if !ok || s.Value != 40_000_000 || s.Route != "/users/:id" || s.Method != "GET" {
				t.Fatalf("handler stack %+v", s)
			}
			if s, _ := stackOf(p, "main.main", "runtime.gcBgMarkWorker"); s.Route != "" || s.Value != 5_000_000 {
				t.Fatalf("gc stack %+v", s)
			}
		}},
		{"samples count × period (eBPF profiler shape)", func(b *otlptest.Builder) {
			st := b.Stack(b.NativeLoc("/usr/lib/libc.so.6", 0x1234), b.Loc("ngx_process_events"), b.Loc("main"))
			b.Resource([]string{"process.executable.name", "nginx", "host.name", "node-1"},
				&profilespb.Profile{SampleType: b.VT("samples", "count"), PeriodType: b.VT("cpu", "nanoseconds"), Period: 50_000_000,
					Samples: []*profilespb.Sample{{StackIndex: st, TimestampsUnixNano: []uint64{t0 + 1e9, t0, t0 + 4e9}}}})
		}, func(t *testing.T, r OTLPResult) {
			if len(r.Profiles) != 1 || r.NoService != 1 {
				t.Fatalf("result %+v", r)
			}
			p := r.Profiles[0]
			if p.Service != "unknown_service:nginx" || p.Runtime != "" || p.Host != "node-1" ||
				p.Start.UnixNano() != int64(t0) || p.End.Sub(p.Start) != 4*time.Second {
				t.Fatalf("identity %+v", p)
			}
			if p.Series.Type != "cpu" || p.Series.Total != 150_000_000 {
				t.Fatalf("series %+v", p.Series)
			}
			if _, ok := stackOf(p, "main", "ngx_process_events", "[libc.so.6]"); !ok {
				t.Fatalf("stacks %+v", p.Series.Stacks)
			}
		}},
		{"pprof-converted cpu: samples/count skipped when cpu/nanoseconds exists", func(b *otlptest.Builder) {
			st := b.Stack(b.Loc("main.main"))
			mk := func(vt *profilespb.ValueType, v int64) *profilespb.Profile {
				return &profilespb.Profile{SampleType: vt, TimeUnixNano: t0, DurationNano: 1e9, PeriodType: b.VT("cpu", "nanoseconds"),
					Period: 10_000_000, Samples: []*profilespb.Sample{{StackIndex: st, Values: []int64{v}}}}
			}
			b.Resource(goRes, mk(b.VT("samples", "count"), 3), mk(b.VT("cpu", "nanoseconds"), 30_000_000))
		}, func(t *testing.T, r OTLPResult) {
			if len(r.Profiles) != 1 || r.Profiles[0].Series.Total != 30_000_000 || r.Unsupported != 1 {
				t.Fatalf("result %+v", r)
			}
		}},
		{"alloc_space on the JVM, inline frames", func(b *otlptest.Builder) {
			// arraycopy and grow are inlined into add (lines: innermost first).
			st := b.Stack(b.Loc("java.util.Arrays.copyOf", "java.util.ArrayList.grow", "java.util.ArrayList.add"), b.Loc("com.acme.Main.main"))
			b.Resource([]string{"service.name", "orders", "process.runtime.name", "OpenJDK Runtime Environment", "deployment.environment", "staging"},
				&profilespb.Profile{SampleType: b.VT("alloc_space", "bytes"), TimeUnixNano: t0, DurationNano: 60e9,
					Samples: []*profilespb.Sample{{StackIndex: st, Values: []int64{4096, 1024}}}})
		}, func(t *testing.T, r OTLPResult) {
			p := r.Profiles[0]
			if p.Runtime != "jvm" || p.Env != "staging" || p.Series.Type != "alloc_space" || p.Series.Unit != "bytes" || p.Series.Total != 5120 {
				t.Fatalf("profile %+v", p)
			}
			if _, ok := stackOf(p, "com.acme.Main.main", "java.util.ArrayList.add", "java.util.ArrayList.grow", "java.util.Arrays.copyOf"); !ok {
				t.Fatalf("stacks %+v", p.Series.Stacks)
			}
		}},
		{"multiple resources and types", func(b *otlptest.Builder) {
			st := b.Stack(b.Loc("main.main"))
			prof := func(typ, unit string) *profilespb.Profile {
				return &profilespb.Profile{SampleType: b.VT(typ, unit), TimeUnixNano: t0, Samples: []*profilespb.Sample{{StackIndex: st, Values: []int64{7}}}}
			}
			b.Resource(goRes, prof("inuse_space", "bytes"), prof("goroutine", "count"))
			b.Resource([]string{"service.name", "worker", "process.runtime.name", "go"}, prof("delay", "nanoseconds"), prof("contentions", "count"))
		}, func(t *testing.T, r OTLPResult) {
			var got []string
			for _, p := range r.Profiles {
				got = append(got, p.Service+"/"+p.Series.Type)
			}
			if !slices.Equal(got, []string{"api/inuse_space", "api/goroutines", "worker/lock"}) || r.Unsupported != 1 {
				t.Fatalf("got %v (%+v)", got, r)
			}
			if p := r.Profiles[0]; !p.Start.Equal(p.End) {
				t.Fatalf("instant profile window %v–%v", p.Start, p.End)
			}
		}},
		{"node frame naming", func(b *otlptest.Builder) {
			st := b.Stack(b.LocFile("", "file:///app/routes.js"), b.LocFile("handle", "/app/server.js"))
			b.Resource([]string{"service.name", "web", "process.runtime.name", "nodejs"},
				&profilespb.Profile{SampleType: b.VT("wall", "nanoseconds"), TimeUnixNano: t0, Samples: []*profilespb.Sample{{StackIndex: st, Values: []int64{1}}}})
		}, func(t *testing.T, r OTLPResult) {
			p := r.Profiles[0]
			if _, ok := stackOf(p, "handle (/app/server.js)", "(anonymous) (/app/routes.js)"); !ok || p.Runtime != "node" || p.Series.Type != "wall" {
				t.Fatalf("profile %+v", p)
			}
		}},
		{"deep stack truncated, missing time, bad indices", func(b *otlptest.Builder) {
			var locs []int32
			for i := range 200 {
				locs = append(locs, b.Loc(fmt.Sprintf("f%03d", i)))
			}
			deep := b.Stack(locs...)
			bad := b.Stack(9999, -1)
			b.Resource(goRes, &profilespb.Profile{SampleType: b.VT("cpu", "nanoseconds"), AttributeIndices: []int32{b.Attr(LabelRoute, "/jobs")},
				Samples: []*profilespb.Sample{
					{StackIndex: deep, Values: []int64{1}},
					{StackIndex: bad, Values: []int64{2}, AttributeIndices: []int32{12345}},
					{StackIndex: 777, Values: []int64{3}},
				}})
		}, func(t *testing.T, r OTLPResult) {
			p := r.Profiles[0]
			if !p.Start.Equal(now) || p.Series.Total != 3 || len(p.Series.Stacks) != 2 {
				t.Fatalf("profile %+v", p)
			}
			for _, s := range p.Series.Stacks {
				if s.Route != "/jobs" {
					t.Fatalf("profile-level route not applied: %+v", s)
				}
				if len(s.Frames) == MaxDepth && (s.Frames[0] != TruncatedFrm || s.Frames[MaxDepth-1] != "f000") {
					t.Fatalf("truncation %v…%v", s.Frames[:2], s.Frames[MaxDepth-1])
				}
			}
			if _, ok := stackOf(p, "[unknown]", "[unknown]"); !ok {
				t.Fatalf("bad indices %+v", p.Series.Stacks)
			}
		}},
		{"collector 0.164 shape: strindex attributes, real entries at index 0", func(b *otlptest.Builder) {
			// The Collector's pprof conversion stores a used stack / location / function at index 0.
			b.D.FunctionTable[0] = &profilespb.Function{NameStrindex: b.Str("runtime.mcall")}
			b.D.LocationTable[0] = &profilespb.Location{Lines: []*profilespb.Line{{FunctionIndex: 0}}}
			b.D.StackTable[0] = &profilespb.Stack{LocationIndices: []int32{0, b.Loc("runtime.goexit")}}
			other := b.Stack(b.Loc("main.work"), b.Loc("runtime.goexit"))
			b.D.AttributeTable[0] = &profilespb.KeyValueAndUnit{KeyStrindex: b.Str(LabelMethod),
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValueStrindex{StringValueStrindex: b.Str("post")}}}
			b.ResourceStrindex([]string{"service.name", "otelcol", "process.runtime.name", "go", "host.name", "h9"},
				&profilespb.Profile{SampleType: b.VT("cpu", "nanoseconds"), TimeUnixNano: t0, Samples: []*profilespb.Sample{
					{StackIndex: 0, Values: []int64{20}},
					{StackIndex: other, Values: []int64{30}, AttributeIndices: []int32{0}},
				}})
		}, func(t *testing.T, r OTLPResult) {
			if len(r.Profiles) != 1 || r.NoService != 0 {
				t.Fatalf("result %+v", r)
			}
			p := r.Profiles[0]
			if p.Service != "otelcol" || p.Runtime != "go" || p.Host != "h9" || p.Series.Total != 50 {
				t.Fatalf("profile %+v", p)
			}
			if s, ok := stackOf(p, "runtime.goexit", "runtime.mcall"); !ok || s.Value != 20 {
				t.Fatalf("index-0 stack %+v", p.Series.Stacks)
			}
			if s, ok := stackOf(p, "runtime.goexit", "main.work"); !ok || s.Method != "POST" {
				t.Fatalf("index-0 attribute %+v", p.Series.Stacks)
			}
		}},
		{"empty and unsupported profiles are skipped", func(b *otlptest.Builder) {
			b.Resource(goRes, &profilespb.Profile{SampleType: b.VT("cpu", "nanoseconds"), TimeUnixNano: t0},
				&profilespb.Profile{SampleType: b.VT("events", "nanoseconds"), TimeUnixNano: t0,
					Samples: []*profilespb.Sample{{StackIndex: b.Stack(b.Loc("x")), Values: []int64{1}}}})
		}, func(t *testing.T, r OTLPResult) {
			if len(r.Profiles) != 0 || r.Empty != 1 || r.Unsupported != 1 {
				t.Fatalf("result %+v", r)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := otlptest.New()
			tc.build(b)
			// Round-trip through the wire format the writer reads.
			pb, err := proto.Marshal(b.Req)
			if err != nil {
				t.Fatal(err)
			}
			req, err := DecodeOTLP(pb, 0)
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, ConvertOTLP(req, now))
		})
	}
}

func TestConvertOTLPNilDictionary(t *testing.T) {
	b := otlptest.New()
	b.Resource(goRes, &profilespb.Profile{SampleType: b.VT("cpu", "nanoseconds"), Samples: []*profilespb.Sample{{StackIndex: 1, Values: []int64{1}}}})
	b.Req.Dictionary = nil
	if r := ConvertOTLP(b.Req, time.Now()); len(r.Profiles) != 0 || r.Unsupported != 1 {
		t.Fatalf("result %+v", r)
	}
}

func TestDecodeOTLP(t *testing.T) {
	b := otlptest.New()
	b.Resource(goRes)
	pb, _ := proto.Marshal(b.Req)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(pb)
	_ = zw.Close()
	tests := []struct {
		name    string
		in      []byte
		max     int64
		wantErr bool
	}{
		{"protobuf", pb, 0, false},
		{"gzip'ed", gz.Bytes(), 0, false},
		{"garbage", []byte("\xff\xff\xff not protobuf"), 0, true},
		{"too large", pb, 4, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := DecodeOTLP(tc.in, tc.max)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if err == nil && len(req.GetResourceProfiles()) != 1 {
				t.Fatalf("req %v", req)
			}
		})
	}
}

func TestRuntimeFromOTel(t *testing.T) {
	for in, want := range map[string]string{
		"go": "go", "OpenJDK Runtime Environment": "jvm", "java": "jvm", "JVM": "jvm", "nodejs": "node", "node": "node",
		".NET": "dotnet", ".NET Framework": "dotnet", "dotnet": "dotnet", "CPython": "python", "python": "python", "PyPy": "python",
		"": "", "ruby": "", "bun": "",
	} {
		if got := RuntimeFromOTel(in); got != want {
			t.Errorf("RuntimeFromOTel(%q) = %q, want %q", in, got, want)
		}
	}
}

// BenchmarkConvertOTLP: an eBPF-profiler-sized request (one host, 20 processes, 2 000 samples of depth 40 over 600 stacks).
func BenchmarkConvertOTLP(b *testing.B) {
	bl := otlptest.New()
	var stacks []int32
	for i := range 600 {
		var locs []int32
		for d := range 40 {
			locs = append(locs, bl.Loc(fmt.Sprintf("pkg%d.fn%d", (i+d)%50, d)))
		}
		stacks = append(stacks, bl.Stack(locs...))
	}
	route := bl.Attr(LabelRoute, "/orders/:id")
	for p := range 20 {
		var samples []*profilespb.Sample
		for i := range 100 {
			samples = append(samples, &profilespb.Sample{StackIndex: stacks[(p*31+i)%len(stacks)], AttributeIndices: []int32{route},
				TimestampsUnixNano: []uint64{t0 + uint64(i)}})
		}
		bl.Resource([]string{"service.name", fmt.Sprintf("svc-%d", p), "host.name", "h1"},
			&profilespb.Profile{SampleType: bl.VT("samples", "count"), PeriodType: bl.VT("cpu", "nanoseconds"), Period: 50_000_000,
				TimeUnixNano: t0, DurationNano: 5e9, Samples: samples})
	}
	pb, _ := proto.Marshal(bl.Req)
	b.SetBytes(int64(len(pb)))
	b.ReportAllocs()
	for b.Loop() {
		req, err := DecodeOTLP(pb, 0)
		if err != nil {
			b.Fatal(err)
		}
		if r := ConvertOTLP(req, time.Now()); len(r.Profiles) != 20 {
			b.Fatal(len(r.Profiles))
		}
	}
}
