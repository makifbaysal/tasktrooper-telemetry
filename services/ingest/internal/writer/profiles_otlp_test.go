package writer

import (
	"bytes"
	"compress/gzip"
	"slices"
	"testing"

	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/ingest/internal/profiling"
	"github.com/tasktrooper/telemetry/services/ingest/internal/profiling/otlptest"
	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

func TestBatchesOTLPProfiles(t *testing.T) {
	const t0 = uint64(1_730_000_000_500_000_000)
	b := otlptest.New()
	h := b.Stack(b.Loc("main.handler"), b.Loc("main.main"))
	route, method := b.Attr("trooper.route", "/orders/:id"), b.Attr("trooper.method", "POST")
	b.Resource([]string{"service.name", "api", "deployment.environment.name", "prod", "service.version", "2.0.0", "host.name", "h1",
		"process.runtime.name", "go"},
		&profilespb.Profile{SampleType: b.VT("cpu", "nanoseconds"), TimeUnixNano: t0, DurationNano: 10e9,
			Samples: []*profilespb.Sample{{StackIndex: h, Values: []int64{40}, AttributeIndices: []int32{route, method}}}},
		&profilespb.Profile{SampleType: b.VT("alloc_space", "bytes"), TimeUnixNano: t0, DurationNano: 10e9,
			Samples: []*profilespb.Sample{{StackIndex: h, Values: []int64{4096}}}})
	b.Resource([]string{"host.name", "h2"}, &profilespb.Profile{SampleType: b.VT("samples", "count"), PeriodType: b.VT("cpu", "nanoseconds"),
		Period: 50_000_000, TimeUnixNano: t0, DurationNano: 5e9, Samples: []*profilespb.Sample{{StackIndex: h, TimestampsUnixNano: []uint64{t0, t0 + 1}}}})
	pb, err := proto.Marshal(b.Req)
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(pb)
	_ = zw.Close()

	otlpHdr := append(slices.Clone(hdrs), topics.HeaderFormat, profiling.FormatOTLP)
	tests := []struct {
		name    string
		value   []byte
		wantErr bool
	}{
		{"protobuf", pb, false},
		{"gzip'ed", gz.Bytes(), false},
		{"garbage", []byte("\xff\xff garbage"), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bt := NewBatches()
			err := bt.Add(raw(topics.Profiles, tc.value, otlpHdr...))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if tc.wantErr {
				return
			}
			if len(bt.Profiles) != 3 || len(bt.ProfileStacks) != 3 {
				t.Fatalf("profiles %d stacks %d", len(bt.Profiles), len(bt.ProfileStacks))
			}
			var got []string
			for _, p := range bt.Profiles {
				got = append(got, p.Service+"/"+p.Host+"/"+p.Type)
				if p.OrgID != 3 || p.TS.UnixMilli() != 1730000000500 {
					t.Fatalf("profile %+v", p)
				}
			}
			if !slices.Equal(got, []string{"api/h1/cpu", "api/h1/alloc_space", "unknown_service/h2/cpu"}) {
				t.Fatalf("profiles %v", got)
			}
			p, s := bt.Profiles[0], bt.ProfileStacks[0]
			if p.Env != "prod" || p.Version != "2.0.0" || p.Runtime != "go" || p.Total != 40 || p.Stacks != 1 || p.EndTS.UnixMilli() != 1730000010500 {
				t.Fatalf("profile %+v", p)
			}
			if s.Route != "/orders/:id" || s.Method != "POST" || s.Value != 40 || !slices.Equal(s.Stack, []string{"main.main", "main.handler"}) ||
				s.TS.Unix() != 1730000000 {
				t.Fatalf("stack %+v", s)
			}
			if e := bt.ProfileStacks[2]; e.Value != 100_000_000 || e.Unit != "nanoseconds" {
				t.Fatalf("eBPF-shaped stack %+v", e)
			}
		})
	}
}
