package writer

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tasktrooper/telemetry/services/ingest/internal/flatten"
	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

func gzipped(s string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(s))
	_ = zw.Close()
	return buf.Bytes()
}

func raw(topic string, v []byte, hdr ...string) *kgo.Record {
	r := &kgo.Record{Topic: topic, Value: v, Timestamp: time.Unix(1_700_000_000, 0)}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Headers = append(r.Headers, kgo.RecordHeader{Key: hdr[i], Value: []byte(hdr[i+1])})
	}
	return r
}

func TestBatchesReplay(t *testing.T) {
	seg := `{"resource":{"service":"shop-web","env":"prod","version":"1.4.0","platform":"web"},"session_id":"sess-1","segment":12,
		"start_ts":1730000000000,"end_ts":1730000004000,"format":"rrweb","has_full_snapshot":true,"view_name":"/checkout",
		"user_id":"u1","anonymous_id":"a1","stats":{"clicks":3,"errors":1,"rage_clicks":70000},"events":[{},{},{}]}`
	tests := []struct {
		name    string
		value   []byte
		wantErr bool
	}{
		{"ok", gzipped(seg), false},
		{"not gzip", []byte(seg), true},
		{"invalid segment", gzipped(`{"session_id":"x"}`), true},
		{"corrupt gzip", gzipped(seg)[:20], true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBatches()
			err := b.Add(raw(topics.Replay, tc.value, append(slices.Clone(hdrs), topics.HeaderSessionID, "sess-1", topics.HeaderSegment, "12")...))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if tc.wantErr {
				if len(b.Replay) != 0 || len(b.Blobs) != 0 {
					t.Fatal("no rows on error")
				}
				return
			}
			r := b.Replay[0]
			want := flatten.ReplayRow{OrgID: 3, ProjectID: 4, SessionID: "sess-1", Segment: 12, Service: "shop-web", Env: "prod", Version: "1.4.0",
				Platform: "web", Format: "rrweb", StartTS: time.UnixMilli(1730000000000).UTC(), EndTS: time.UnixMilli(1730000004000).UTC(),
				Events: 3, Bytes: uint32(len(tc.value)), BlobKey: "3/4/2024-10-27/sess-1/000012.json.gz", HasFullSnapshot: 1,
				Clicks: 3, Errors: 1, RageClicks: 65535, ViewName: "/checkout", UserID: "u1", AnonymousID: "a1",
				ReceivedAt: time.UnixMilli(1700000000123).UTC()}
			if !r.StartTS.Equal(want.StartTS) || !r.ReceivedAt.Equal(want.ReceivedAt) {
				t.Fatalf("times %v %v", r.StartTS, r.ReceivedAt)
			}
			r.StartTS, r.EndTS, r.ReceivedAt = want.StartTS, want.EndTS, want.ReceivedAt
			if r != want {
				t.Fatalf("row\n%+v\nwant\n%+v", r, want)
			}
			if len(b.Blobs) != 1 || b.Blobs[0].Key != want.BlobKey || !bytes.Equal(b.Blobs[0].Body, tc.value) {
				t.Fatalf("blobs %+v", b.Blobs)
			}
			if n := countCols(flatten.ReplayColumns); n != len(r.Values()) {
				t.Fatalf("%d columns vs %d values", n, len(r.Values()))
			}
			b.Reset()
			if len(b.Blobs) != 0 || b.blobBytes != 0 || b.Rows() != 0 {
				t.Fatal("reset")
			}
		})
	}
}

func TestBatchesProfiles(t *testing.T) {
	ph := func(kv ...string) []string {
		base := []string{topics.HeaderService, "api", topics.HeaderEnv, "prod", topics.HeaderVersion, "2.0.0", topics.HeaderHost, "h1",
			topics.HeaderRuntime, "jvm", topics.HeaderStart, "1730000000500", topics.HeaderEnd, "1730000010500",
			topics.HeaderFormat, "folded", topics.HeaderType, "cpu", topics.HeaderUnit, "nanoseconds"}
		m := map[string]string{}
		for i := 0; i+1 < len(base); i += 2 {
			m[base[i]] = base[i+1]
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		out := slices.Clone(hdrs)
		for k, v := range m {
			out = append(out, k, v)
		}
		return out
	}
	folded := "# trooper.route=/orders trooper.method=GET\ncom.acme.Main.main;com.acme.Api.get 30\ncom.acme.Main.main;com.acme.Api.get 10\n#\ncom.acme.Main.main 0\ncom.acme.Main.main;com.acme.Gc.run 5\n"
	tests := []struct {
		name       string
		value      []byte
		hdr        []string
		wantErr    bool
		wantStacks int
		wantTotal  int64
	}{
		{"folded", []byte(folded), ph(), false, 2, 45},
		{"folded gzip'ed", gzipped(folded), ph(), false, 2, 45},
		{"missing start", []byte(folded), ph(topics.HeaderStart, ""), true, 0, 0},
		{"end before start", []byte(folded), ph(topics.HeaderEnd, "1"), true, 0, 0},
		{"missing service", []byte(folded), ph(topics.HeaderService, ""), true, 0, 0},
		{"bad body", []byte("x"), ph(), true, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBatches()
			err := b.Add(raw(topics.Profiles, tc.value, tc.hdr...))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if tc.wantErr {
				return
			}
			if len(b.Profiles) != 1 || len(b.ProfileStacks) != tc.wantStacks {
				t.Fatalf("profiles %d stacks %d", len(b.Profiles), len(b.ProfileStacks))
			}
			p := b.Profiles[0]
			if p.Total != tc.wantTotal || p.Stacks != uint32(tc.wantStacks) || p.Service != "api" || p.Runtime != "jvm" || p.Type != "cpu" ||
				p.Unit != "nanoseconds" || p.TS.UnixMilli() != 1730000000500 || p.EndTS.UnixMilli() != 1730000010500 || p.OrgID != 3 {
				t.Fatalf("profile %+v", p)
			}
			s := b.ProfileStacks[0]
			if s.Route != "/orders" || s.Method != "GET" || s.Value != 40 || !slices.Equal(s.Stack, []string{"com.acme.Main.main", "com.acme.Api.get"}) ||
				s.TS.Unix() != 1730000000 || s.Host != "h1" {
				t.Fatalf("stack %+v", s)
			}
			if n := countCols(flatten.ProfileStackColumns); n != len(s.Values()) {
				t.Fatalf("%d columns vs %d values", n, len(s.Values()))
			}
			if n := countCols(flatten.ProfileColumns); n != len(p.Values()) {
				t.Fatalf("%d columns vs %d values", n, len(p.Values()))
			}
			var names []string
			for _, tb := range b.tables() {
				names = append(names, tb.table)
			}
			if !slices.Contains(names, "profile_stacks") || !slices.Contains(names, "profiles") {
				t.Fatalf("tables %v", names)
			}
		})
	}
}

func llmTrace(trace byte, errSpan bool) *coltracepb.ExportTraceServiceRequest {
	sv := func(s string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
	}
	sp := &tracepb.Span{TraceId: bytes.Repeat([]byte{trace}, 16), SpanId: []byte{1, 2, 3, 4, 5, 6, 7, trace}, Kind: tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: 1, EndTimeUnixNano: 2, Attributes: []*commonpb.KeyValue{
			{Key: "gen_ai.operation.name", Value: sv("chat")}, {Key: "gen_ai.provider.name", Value: sv("openai")},
			{Key: "gen_ai.input.messages", Value: sv("[]")}}}
	if errSpan {
		sp.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR}
	}
	return &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{sp}}}}}}
}

func TestSampleDropsContentOfDroppedTraces(t *testing.T) {
	b := NewBatches()
	for i := byte(1); i <= 60; i++ {
		if err := b.Add(rec(topics.Traces, llmTrace(i, i == 7), hdrs...)); err != nil {
			t.Fatal(err)
		}
	}
	// content of a trace with no spans in this window (e.g. from a log event) must stay
	b.LLMContent = append(b.LLMContent, flatten.LLMContentRow{TraceID: bytes.Repeat([]byte{200}, 16), Input: "x"})
	if len(b.LLMContent) != 61 {
		t.Fatalf("content rows %d", len(b.LLMContent))
	}
	b.Sample(TailSampler{Rate: 0.3})
	kept := map[byte]bool{}
	for _, s := range b.Spans {
		kept[s.TraceID[0]] = true
	}
	if !kept[7] || len(b.Spans) == 60 || len(b.Spans) == 0 {
		t.Fatalf("sampling kept %d spans (error trace kept: %v)", len(b.Spans), kept[7])
	}
	orphan := false
	for _, c := range b.LLMContent {
		if c.TraceID[0] == 200 {
			orphan = true
			continue
		}
		if !kept[c.TraceID[0]] {
			t.Fatalf("content of dropped trace %d kept", c.TraceID[0])
		}
	}
	if !orphan || len(b.LLMContent) != len(b.Spans)+1 {
		t.Fatalf("content %d spans %d orphan %v", len(b.LLMContent), len(b.Spans), orphan)
	}
	// second call is a no-op (already sampled)
	n := len(b.LLMContent)
	b.Sample(TailSampler{Rate: 0.3})
	if len(b.LLMContent) != n {
		t.Fatal("content re-sampled")
	}
}

type flakyStore struct {
	mu    sync.Mutex
	fails int
	puts  map[string][]byte
}

func (f *flakyStore) Put(_ context.Context, key string, body []byte, ct, ce string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ct != "application/json" || ce != "gzip" {
		return errors.New("bad content headers")
	}
	if f.fails > 0 {
		f.fails--
		return errors.New("s3 down")
	}
	if f.puts == nil {
		f.puts = map[string][]byte{}
	}
	f.puts[key] = body
	return nil
}

func TestUploadBlobs(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := &flakyStore{fails: 2}
	w := &Writer{cfg: Config{Blobs: st, BlobWorkers: 2, RetryMax: 10 * time.Millisecond}, log: log, b: NewBatches()}
	for i := range 5 {
		w.b.Blobs = append(w.b.Blobs, Blob{Key: string(rune('a' + i)), Body: []byte{byte(i)}})
	}
	if err := w.uploadBlobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.puts) != 5 {
		t.Fatalf("puts %d", len(st.puts))
	}
	// cancelled while S3 keeps failing → error (nothing committed)
	st.fails = 1 << 30
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := w.uploadBlobs(ctx); err == nil {
		t.Fatal("want error")
	}
	// no store: replay rows are dropped, not retried forever
	w2 := &Writer{cfg: Config{}, log: log, b: NewBatches()}
	w2.b.Blobs = []Blob{{Key: "k"}}
	w2.b.Replay = []flatten.ReplayRow{{}}
	if err := w2.uploadBlobs(context.Background()); err != nil || len(w2.b.Replay) != 0 {
		t.Fatalf("%v %d", err, len(w2.b.Replay))
	}
}
