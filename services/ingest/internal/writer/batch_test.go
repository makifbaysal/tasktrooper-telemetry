package writer

import (
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/ingest/internal/flatten"
	"github.com/tasktrooper/telemetry/services/pkg/rumpb"
	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

func rec(topic string, m proto.Message, hdr ...string) *kgo.Record {
	v, _ := proto.Marshal(m)
	r := &kgo.Record{Topic: topic, Value: v, Timestamp: time.Unix(1_700_000_000, 0)}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Headers = append(r.Headers, kgo.RecordHeader{Key: hdr[i], Value: []byte(hdr[i+1])})
	}
	return r
}

var hdrs = []string{topics.HeaderOrgID, "3", topics.HeaderProjectID, "4", topics.HeaderReceivedAt, "1700000000123", topics.HeaderUserAgent, "okhttp/4"}

func TestBatchesAdd(t *testing.T) {
	b := NewBatches()
	tr := &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
		{TraceId: make([]byte, 16), SpanId: []byte{1, 2, 3, 4, 5, 6, 7, 8}, Kind: tracepb.Span_SPAN_KIND_SERVER},
		{TraceId: make([]byte, 16), SpanId: []byte{1, 2, 3, 4, 5, 6, 7, 9}, Kind: tracepb.Span_SPAN_KIND_CLIENT},
	}}}}}}
	cpu := &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
		{Name: "trooper.broker.lag", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
			Value: &metricspb.NumberDataPoint_AsInt{AsInt: 5}, Attributes: []*commonpb.KeyValue{}}}}}},
	}}}}}}
	rum := &rumpb.RumBatch{Resource: &rumpb.Resource{App: "a"}, Events: []*rumpb.RumEvent{{Type: rumpb.EventType_ACTION}}}

	for _, r := range []*kgo.Record{rec(topics.Traces, tr, hdrs...), rec(topics.Metrics, cpu, hdrs...), rec(topics.RUM, rum, hdrs...)} {
		if err := b.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Add(rec(topics.Traces, tr)); err == nil {
		t.Error("record without tenant headers must be rejected")
	}
	if err := b.Add(&kgo.Record{Topic: topics.Logs, Value: []byte{0xff}, Headers: rec("", tr, hdrs...).Headers}); err == nil {
		t.Error("undecodable record must be rejected")
	}
	if len(b.Spans) != 2 || b.Spans[0].OrgID != 3 || b.Spans[0].ProjectID != 4 {
		t.Fatalf("spans: %d %+v", len(b.Spans), b.Spans)
	}
	if len(b.Rum) != 1 || b.Rum[0].ReceivedAt.UnixMilli() != 1700000000123 || b.Rum[0].OS != "" || b.Rum[0].DeviceType == "" {
		t.Fatalf("rum: %+v", b.Rum)
	}
	if b.Rows() != 2 {
		t.Errorf("Rows=%d", b.Rows())
	}

	tables := b.tables()
	got := map[string]int{}
	for _, tb := range tables {
		got[tb.table] = tb.n
		for i := range tb.n {
			if len(tb.row(i)) == 0 {
				t.Fatal("empty row")
			}
		}
	}
	want := map[string]int{"spans": 2, "broker_lag_1m": 1, "rum_events": 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s=%d want %d (all: %v)", k, got[k], v, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("unexpected tables: %v", got)
	}
	// Column count matches the column list (guards against drift with the DDL list).
	for _, c := range []struct {
		cols string
		vals []any
	}{
		{flatten.SpanColumns, b.Spans[0].Values()},
		{flatten.RumColumns, b.Rum[0].Values()},
		{flatten.LogColumns, (&flatten.LogRow{}).Values()},
		{flatten.MetricColumns, (&flatten.MetricRow{}).Values()},
		{flatten.DBColumns, (&flatten.DBRow{}).Values()},
		{flatten.DBQueryColumns, (&flatten.DBQueryRow{}).Values()},
		{flatten.BrokerLagColumns, (&flatten.BrokerLagRow{}).Values()},
		{flatten.BrokerGroupColumns, (&flatten.BrokerGroupRow{}).Values()},
		{flatten.BrokerNodeColumns, (&flatten.BrokerNodeRow{}).Values()},
	} {
		if n := countCols(c.cols); n != len(c.vals) {
			t.Errorf("%d columns vs %d values: %s", n, len(c.vals), c.cols)
		}
	}

	b.Reset()
	if b.Rows() != 0 || len(b.tables()) != 0 {
		t.Error("reset")
	}
}

func countCols(s string) int {
	n := 1
	for _, c := range s {
		if c == ',' {
			n++
		}
	}
	return n
}
