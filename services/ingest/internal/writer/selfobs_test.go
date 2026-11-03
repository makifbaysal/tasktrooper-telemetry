package writer

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/twmb/franz-go/pkg/kgo"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tasktrooper/telemetry/services/ingest/internal/selfobs"
	"github.com/tasktrooper/telemetry/services/ingest/internal/selfobs/selfobstest"
	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

type countingInserter struct {
	mu   sync.Mutex
	rows map[string]int
}

func (c *countingInserter) Insert(_ context.Context, table, _ string, n int, _ func(int) []any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rows == nil {
		c.rows = map[string]int{}
	}
	c.rows[table] += n
	return nil
}

func projectRec(project string, value []byte) *kgo.Record {
	r := &kgo.Record{Topic: topics.Traces, Value: value}
	for _, h := range [][2]string{{topics.HeaderOrgID, "1"}, {topics.HeaderProjectID, project}, {topics.HeaderReceivedAt, "1700000000123"}} {
		r.Headers = append(r.Headers, kgo.RecordHeader{Key: h[0], Value: []byte(h[1])})
	}
	return r
}

func TestWriterSelfTelemetryLoopGuard(t *testing.T) {
	spans := &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
		{TraceId: make([]byte, 16), SpanId: []byte{1, 2, 3, 4, 5, 6, 7, 8}, Kind: tracepb.Span_SPAN_KIND_SERVER},
	}}}}}}
	good := rec(topics.Traces, spans).Value
	bad := []byte{0xff, 0xff, 0xff} // undecodable → warn log
	tests := []struct {
		name      string
		tel       bool
		self      uint32
		records   []*kgo.Record
		wantSpans []string
		wantLogs  int // exported OTel "skipping undecodable record" logs
	}{
		{name: "self only: no spans, no otel logs", tel: true, self: 2,
			records: []*kgo.Record{projectRec("2", good), projectRec("2", bad)}},
		{name: "mixed batch traced, self log suppressed", tel: true, self: 2,
			records:   []*kgo.Record{projectRec("2", good), projectRec("9", good), projectRec("2", bad)},
			wantSpans: []string{"INSERT spans", "process flush"}},
		{name: "customer log exported", tel: true, self: 2,
			records:   []*kgo.Record{projectRec("9", good), projectRec("9", bad)},
			wantSpans: []string{"INSERT spans", "process flush"}, wantLogs: 1},
		{name: "guard off", tel: true, self: 0,
			records:   []*kgo.Record{projectRec("2", good)},
			wantSpans: []string{"INSERT spans", "process flush"}},
		{name: "telemetry off", tel: false, self: 2,
			records: []*kgo.Record{projectRec("9", good), projectRec("9", bad)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := selfobstest.Install(t)
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			if tt.tel {
				log = slog.New(selfobs.LogHandler(slog.NewTextHandler(io.Discard, nil), "tt-ingest-writer"))
			}
			cl, err := kgo.NewClient(kgo.SeedBrokers("127.0.0.1:1"))
			if err != nil {
				t.Fatal(err)
			}
			defer cl.Close()
			cfg := Config{Group: "tt-ingest-writer"}
			if tt.tel {
				cfg.Tel = selfobs.New(nil, tt.self)
			}
			ins := &countingInserter{}
			w := &Writer{cfg: cfg, ins: ins, log: log, cl: cl, b: NewBatches(), pending: map[tp]*kgo.Record{}}
			for _, r := range tt.records {
				w.addRecord(context.Background(), r)
			}
			if err := w.flushLocked(context.Background()); err != nil {
				t.Fatal(err)
			}
			if ins.rows["spans"] == 0 {
				t.Fatal("rows must be inserted regardless of telemetry")
			}
			got := rec.SpanNames()
			sort.Strings(got)
			if !slices.Equal(got, tt.wantSpans) {
				t.Fatalf("spans = %q, want %q", got, tt.wantSpans)
			}
			n := 0
			for _, b := range rec.Logs.Bodies() {
				if b == "skipping undecodable record" {
					n++
				}
			}
			if n != tt.wantLogs {
				t.Fatalf("otel logs = %q, want %d undecodable warnings", rec.Logs.Bodies(), tt.wantLogs)
			}
			if w.stats.Records != 0 || w.stats.Self != 0 || w.stats.Topics != nil {
				t.Fatalf("stats not reset: %+v", w.stats)
			}
			for _, s := range rec.Spans.Ended() {
				if s.Name() == "process flush" {
					for _, kv := range s.Attributes() {
						if kv.Key == "messaging.destination.name" && kv.Value.AsString() != topics.Traces {
							t.Fatalf("flush topic %q", kv.Value.AsString())
						}
					}
				}
				if s.Name() == "INSERT spans" {
					attrs := map[string]string{}
					for _, kv := range s.Attributes() {
						attrs[string(kv.Key)] = kv.Value.Emit()
					}
					if attrs["db.system"] != "clickhouse" || attrs["db.operation.batch.size"] != "2" && attrs["db.operation.batch.size"] != "1" {
						t.Fatalf("insert attrs %v", attrs)
					}
				}
			}
		})
	}
}
