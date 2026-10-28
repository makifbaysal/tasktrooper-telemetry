package profiling

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/pprof/profile"
)

// testProfile builds a pprof profile: main → handler → work (work inlined into handler at one location).
func testProfile(sampleTypes [][2]string, values [][]int64, labels []map[string][]string, period *profile.ValueType) *profile.Profile {
	fMain := &profile.Function{ID: 1, Name: "main.main", Filename: "/app/main.go"}
	fHandler := &profile.Function{ID: 2, Name: "github.com/acme/api.(*Server).handle", Filename: "/app/server.go"}
	fWork := &profile.Function{ID: 3, Name: "github.com/acme/api.work", Filename: "/app/work.go"}
	lMain := &profile.Location{ID: 1, Line: []profile.Line{{Function: fMain, Line: 10}}}
	// inlined: work (callee) first, handler last
	lHandler := &profile.Location{ID: 2, Line: []profile.Line{{Function: fWork, Line: 5}, {Function: fHandler, Line: 20}}}
	lMain2 := &profile.Location{ID: 3, Line: []profile.Line{{Function: fMain, Line: 11}}}
	p := &profile.Profile{Function: []*profile.Function{fMain, fHandler, fWork}, Location: []*profile.Location{lMain, lHandler, lMain2},
		PeriodType: period, Period: 10_000_000, TimeNanos: 1, DurationNanos: 1}
	for _, st := range sampleTypes {
		p.SampleType = append(p.SampleType, &profile.ValueType{Type: st[0], Unit: st[1]})
	}
	for i, v := range values {
		s := &profile.Sample{Value: v, Location: []*profile.Location{lHandler, lMain}}
		if i == 2 {
			s.Location = []*profile.Location{lMain2} // a different stack
		}
		if i < len(labels) {
			s.Label = labels[i]
		}
		p.Sample = append(p.Sample, s)
	}
	return p
}

func encode(t testing.TB, p *profile.Profile, gzipped bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	var err error
	if gzipped {
		err = p.Write(&buf)
	} else {
		err = p.WriteUncompressed(&buf)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func byType(ss []Series) map[string]Series {
	m := map[string]Series{}
	for _, s := range ss {
		m[s.Type] = s
	}
	return m
}

func TestPprofSampleTypes(t *testing.T) {
	route := []map[string][]string{{"trooper.route": {"/users/:id"}, "trooper.method": {"get"}}}
	tests := []struct {
		name      string
		types     [][2]string
		values    [][]int64
		opt       Options
		period    *profile.ValueType
		wantTypes map[string]int64 // type → total
		wantUnit  map[string]string
		wantErr   bool
	}{
		{name: "go cpu skips samples/count", types: [][2]string{{"samples", "count"}, {"cpu", "nanoseconds"}},
			values: [][]int64{{1, 10}, {2, 20}, {3, 30}}, wantTypes: map[string]int64{"cpu": 60}, wantUnit: map[string]string{"cpu": "nanoseconds"}},
		{name: "go heap", types: [][2]string{{"alloc_objects", "count"}, {"alloc_space", "bytes"}, {"inuse_objects", "count"}, {"inuse_space", "bytes"}},
			values:    [][]int64{{1, 100, 0, 0}, {1, 50, 1, 50}, {2, 10, 0, 0}},
			wantTypes: map[string]int64{"alloc_objects": 4, "alloc_space": 160, "inuse_objects": 1, "inuse_space": 50}},
		{name: "goroutines", types: [][2]string{{"goroutine", "count"}}, values: [][]int64{{3}, {4}, {1}}, wantTypes: map[string]int64{"goroutines": 8}},
		{name: "mutex → lock, contentions skipped", types: [][2]string{{"contentions", "count"}, {"delay", "nanoseconds"}},
			values: [][]int64{{1, 1000}, {1, 500}, {1, 0}}, wantTypes: map[string]int64{"lock": 1500}},
		{name: "block via ?type=block", types: [][2]string{{"contentions", "count"}, {"delay", "nanoseconds"}}, opt: Options{Type: "block"},
			values: [][]int64{{1, 1000}}, wantTypes: map[string]int64{"block": 1000}},
		{name: "cpu from samples × period", types: [][2]string{{"samples", "count"}}, period: &profile.ValueType{Type: "cpu", Unit: "nanoseconds"},
			values: [][]int64{{2}, {1}}, wantTypes: map[string]int64{"cpu": 30_000_000}},
		{name: "wall", types: [][2]string{{"sample", "count"}, {"wall", "nanoseconds"}}, values: [][]int64{{1, 7}}, wantTypes: map[string]int64{"wall": 7}},
		{name: "unsupported", types: [][2]string{{"samples", "count"}}, values: [][]int64{{1}}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, gz := range []bool{false, true} {
				body := encode(t, testProfile(tc.types, tc.values, route, tc.period), gz)
				got, err := Parse(body, tc.opt)
				if tc.wantErr {
					if err == nil {
						t.Fatal("want error")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				m := byType(got)
				if len(m) != len(tc.wantTypes) {
					t.Fatalf("types = %v, want %v", m, tc.wantTypes)
				}
				for typ, total := range tc.wantTypes {
					if m[typ].Total != total {
						t.Errorf("%s total = %d, want %d", typ, m[typ].Total, total)
					}
					if u := tc.wantUnit[typ]; u != "" && m[typ].Unit != u {
						t.Errorf("%s unit = %q", typ, m[typ].Unit)
					}
					if m[typ].Unit != DefaultUnit(typ) {
						t.Errorf("%s unit = %q, want %q", typ, m[typ].Unit, DefaultUnit(typ))
					}
				}
			}
		})
	}
}

func TestPprofStacksAndLabels(t *testing.T) {
	route := []map[string][]string{{"trooper.route": {"/users/:id"}, "trooper.method": {"get"}, "other": {"x"}}, nil, nil}
	p := testProfile([][2]string{{"cpu", "nanoseconds"}}, [][]int64{{10}, {5}, {1}}, route, nil)
	got, err := Parse(encode(t, p, true), Options{Runtime: "go"})
	if err != nil {
		t.Fatal(err)
	}
	s := got[0]
	want := []Stack{
		{Route: "/users/:id", Method: "GET", Frames: []string{"main.main", "github.com/acme/api.(*Server).handle", "github.com/acme/api.work"}, Value: 10},
		{Frames: []string{"main.main", "github.com/acme/api.(*Server).handle", "github.com/acme/api.work"}, Value: 5},
		{Frames: []string{"main.main"}, Value: 1},
	}
	if len(s.Stacks) != len(want) {
		t.Fatalf("stacks = %+v", s.Stacks)
	}
	for i := range want {
		if g := s.Stacks[i]; g.Route != want[i].Route || g.Method != want[i].Method || g.Value != want[i].Value || !slices.Equal(g.Frames, want[i].Frames) {
			t.Errorf("stack %d = %+v, want %+v", i, g, want[i])
		}
	}
}

func TestPprofNodeFrames(t *testing.T) {
	f1 := &profile.Function{ID: 1, Name: "handler", Filename: "file:///app/src/routes.js"}
	f2 := &profile.Function{ID: 2, Name: "", Filename: "/app/src/util.js"}
	l1 := &profile.Location{ID: 1, Line: []profile.Line{{Function: f1, Line: 3, Column: 7}}}
	l2 := &profile.Location{ID: 2, Line: []profile.Line{{Function: f2, Line: 9}}}
	l3 := &profile.Location{ID: 3, Mapping: &profile.Mapping{ID: 1, File: "/usr/lib/libc.so.6"}, Address: 0x1234}
	p := &profile.Profile{SampleType: []*profile.ValueType{{Type: "wall", Unit: "nanoseconds"}}, Function: []*profile.Function{f1, f2},
		Mapping: []*profile.Mapping{l3.Mapping}, Location: []*profile.Location{l1, l2, l3},
		Sample: []*profile.Sample{{Value: []int64{5}, Location: []*profile.Location{l3, l2, l1}}}}
	got, err := Parse(encode(t, p, false), Options{Runtime: "node"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"handler (/app/src/routes.js)", "(anonymous) (/app/src/util.js)", "[libc.so.6]"}
	if g := got[0].Stacks[0].Frames; !slices.Equal(g, want) {
		t.Fatalf("frames = %q, want %q", g, want)
	}
}

func TestFolded(t *testing.T) {
	body := "main;a;b 10\n" +
		"main;a;b 5\n" +
		"# trooper.route=/orders/:id trooper.method=post\n" +
		"main;a;b 7\r\n" +
		"main;c 3\n" +
		"#\n" +
		"main;c 2\n" +
		"main;zero 0\n" +
		"\n" +
		"  main ; d  1\n"
	got, err := Parse([]byte(body), Options{Format: FormatFolded, Type: "cpu", Unit: "nanoseconds"})
	if err != nil {
		t.Fatal(err)
	}
	s := got[0]
	if s.Type != "cpu" || s.Unit != "nanoseconds" || s.Total != 28 {
		t.Fatalf("series = %s/%s total %d", s.Type, s.Unit, s.Total)
	}
	want := []Stack{
		{Frames: []string{"main", "a", "b"}, Value: 15},
		{Route: "/orders/:id", Method: "POST", Frames: []string{"main", "a", "b"}, Value: 7},
		{Route: "/orders/:id", Method: "POST", Frames: []string{"main", "c"}, Value: 3},
		{Frames: []string{"main", "c"}, Value: 2},
		{Frames: []string{"main", "d"}, Value: 1},
	}
	if len(s.Stacks) != len(want) {
		t.Fatalf("stacks = %+v", s.Stacks)
	}
	for i := range want {
		if g := s.Stacks[i]; g.Route != want[i].Route || g.Method != want[i].Method || g.Value != want[i].Value || !slices.Equal(g.Frames, want[i].Frames) {
			t.Errorf("stack %d = %+v, want %+v", i, g, want[i])
		}
	}
}

func TestFoldedErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		opt  Options
	}{
		{"no value", "main;a\n", Options{Format: FormatFolded, Type: "cpu"}},
		{"bad value", "main;a x\n", Options{Format: FormatFolded, Type: "cpu"}},
		{"negative", "main;a -1\n", Options{Format: FormatFolded, Type: "cpu"}},
		{"unknown type", "main;a 1\n", Options{Format: FormatFolded, Type: "heap"}},
		{"unknown format", "main;a 1\n", Options{Format: "jfr", Type: "cpu"}},
		{"bad pprof", "not a profile", Options{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.body), tc.opt); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestDepthAndStackLimits(t *testing.T) {
	t.Run("depth", func(t *testing.T) {
		frames := make([]string, 200)
		for i := range frames {
			frames[i] = fmt.Sprintf("f%d", i)
		}
		got, err := Parse([]byte(strings.Join(frames, ";")+" 1\n"), Options{Format: FormatFolded, Type: "cpu"})
		if err != nil {
			t.Fatal(err)
		}
		fr := got[0].Stacks[0].Frames
		if len(fr) != MaxDepth || fr[0] != TruncatedFrm || fr[1] != "f73" || fr[MaxDepth-1] != "f199" {
			t.Fatalf("len %d, first %q %q, last %q", len(fr), fr[0], fr[1], fr[len(fr)-1])
		}
	})
	t.Run("stacks", func(t *testing.T) {
		var b strings.Builder
		n := MaxStacks + 500
		for i := range n {
			if i == MaxStacks+100 {
				b.WriteString("# trooper.route=/r trooper.method=GET\n")
			}
			fmt.Fprintf(&b, "main;f%d %d\n", i, n-i) // values descending: the last 500 are the smallest
		}
		got, err := Parse([]byte(b.String()), Options{Format: FormatFolded, Type: "alloc_space"})
		if err != nil {
			t.Fatal(err)
		}
		s := got[0]
		var sum, other int64
		var others []Stack
		for _, st := range s.Stacks {
			sum += st.Value
			if len(st.Frames) == 1 && st.Frames[0] == OtherFrm {
				others = append(others, st)
				other += st.Value
			}
		}
		if len(s.Stacks) != MaxStacks+2 || len(others) != 2 {
			t.Fatalf("stacks %d, other rows %d", len(s.Stacks), len(others))
		}
		if sum != s.Total {
			t.Fatalf("sum %d != total %d", sum, s.Total)
		}
		if want := int64(500 * 501 / 2); other != want {
			t.Fatalf("other = %d, want %d", other, want)
		}
		if s.Unit != "bytes" {
			t.Fatalf("unit %q", s.Unit)
		}
	})
}

func TestGunzipLimit(t *testing.T) {
	var buf bytes.Buffer
	p := testProfile([][2]string{{"cpu", "nanoseconds"}}, [][]int64{{1}}, nil, nil)
	_ = p.Write(&buf)
	if _, err := Parse(buf.Bytes(), Options{MaxSize: 10}); err != ErrTooLarge {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func BenchmarkParsePprof(b *testing.B) {
	// ~2000 samples over 400 distinct 30-frame stacks, gzip'ed (a typical 10 s Go CPU profile).
	var fns []*profile.Function
	var locs []*profile.Location
	for i := range 200 {
		f := &profile.Function{ID: uint64(i + 1), Name: fmt.Sprintf("github.com/acme/svc/internal/pkg%d.(*Type).Method%d", i%17, i)}
		fns = append(fns, f)
		locs = append(locs, &profile.Location{ID: uint64(i + 1), Line: []profile.Line{{Function: f, Line: int64(i)}}})
	}
	p := &profile.Profile{SampleType: []*profile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}},
		Function: fns, Location: locs, PeriodType: &profile.ValueType{Type: "cpu", Unit: "nanoseconds"}, Period: 10_000_000}
	for i := range 2000 {
		st := make([]*profile.Location, 30)
		for j := range st {
			st[j] = locs[(i%400*7+j*13)%len(locs)]
		}
		var lbl map[string][]string
		if i%3 == 0 {
			lbl = map[string][]string{"trooper.route": {fmt.Sprintf("/r/%d", i%10)}, "trooper.method": {"GET"}}
		}
		p.Sample = append(p.Sample, &profile.Sample{Value: []int64{1, 10_000_000}, Location: st, Label: lbl})
	}
	body := encode(b, p, true)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Parse(body, Options{Runtime: "go"}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseFolded(b *testing.B) {
	var sb strings.Builder
	for i := range 2000 {
		if i%500 == 0 {
			fmt.Fprintf(&sb, "# trooper.route=/r/%d trooper.method=GET\n", i/500)
		}
		for j := range 30 {
			if j > 0 {
				sb.WriteByte(';')
			}
			fmt.Fprintf(&sb, "com.acme.svc.pkg%d.Type.method%d", j%17, (i%400*7+j*13)%200)
		}
		fmt.Fprintf(&sb, " %d\n", 10_000_000)
	}
	body := []byte(sb.String())
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Parse(body, Options{Format: FormatFolded, Type: "cpu", Unit: "nanoseconds", Runtime: "jvm"}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestDoubleGzip(t *testing.T) {
	var inner, outer bytes.Buffer
	_ = testProfile([][2]string{{"cpu", "nanoseconds"}}, [][]int64{{5}}, nil, nil).Write(&inner) // gzip'ed pprof
	zw := gzip.NewWriter(&outer)
	_, _ = zw.Write(inner.Bytes()) // + Content-Encoding: gzip on the wire
	_ = zw.Close()
	got, err := Parse(outer.Bytes(), Options{})
	if err != nil || got[0].Total != 5 {
		t.Fatalf("%v %+v", err, got)
	}
}
