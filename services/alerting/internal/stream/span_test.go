package stream

import (
	"context"
	"testing"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/trace"

	"github.com/makifbaysal/tasktrooper-telemetry/sdks/go/troopertest"

	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

func rec(topic, project string) *kgo.Record {
	return &kgo.Record{Topic: topic, Headers: []kgo.RecordHeader{{Key: topics.HeaderProjectID, Value: []byte(project)}}}
}

func TestHandleTopicSpans(t *testing.T) {
	tests := []struct {
		name      string
		self      int64
		recs      []*kgo.Record
		wantSpans int
		wantCount string
	}{
		{name: "customer batch", self: 2, recs: []*kgo.Record{rec(topics.Logs, "9"), rec(topics.Logs, "2")}, wantSpans: 1, wantCount: "2"},
		{name: "self-only batch", self: 2, recs: []*kgo.Record{rec(topics.Logs, "2"), rec(topics.Logs, "2")}},
		{name: "guard off", recs: []*kgo.Record{rec(topics.Logs, "2")}, wantSpans: 1, wantCount: "1"},
		{name: "empty", self: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "tt-alert-engine")
			p := NewProcessor(&fakeEngine{}, nil, nil)
			p.Refresh()
			p.HandleTopic(context.Background(), Config{SelfProject: tt.self}, topics.Logs, tt.recs)
			spans := h.Spans()
			if len(spans) != tt.wantSpans {
				t.Fatalf("spans = %d, want %d", len(spans), tt.wantSpans)
			}
			if tt.wantSpans == 0 {
				return
			}
			s := spans[0]
			if s.SpanKind() != trace.SpanKindConsumer || s.Name() != "process "+topics.Logs ||
				troopertest.AttrString(s, "messaging.destination.name") != topics.Logs ||
				troopertest.AttrString(s, "messaging.consumer.group.name") != Group ||
				troopertest.AttrString(s, "trooper.process") != Group {
				t.Fatalf("span %s attrs %v", s.Name(), s.Attributes())
			}
			if v, _ := troopertest.Attr(s, "messaging.batch.message_count"); v.Emit() != tt.wantCount {
				t.Fatalf("count %v", v.Emit())
			}
		})
	}
}
