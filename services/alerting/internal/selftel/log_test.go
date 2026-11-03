package selftel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	trooper "github.com/makifbaysal/tasktrooper-telemetry/sdks/go"
	"github.com/makifbaysal/tasktrooper-telemetry/sdks/go/troopertest"
)

func TestEntryRoute(t *testing.T) {
	tests := []struct {
		name       string
		ctx        func() context.Context
		wantRoute  string
		wantMethod string
	}{
		{name: "no entry", ctx: context.Background},
		{name: "job entry", ctx: func() context.Context {
			ctx, _ := StartJob(context.Background(), "notify.deliver")
			return ctx
		}, wantRoute: "notify.deliver", wantMethod: "JOB"},
		{name: "consumer entry", ctx: func() context.Context {
			ctx, _ := trooper.StartConsumer(context.Background(), nil, trooper.Message{System: "kafka", Destination: "tt.otlp.logs", ConsumerGroup: "tt-alert-stream", Partition: -1})
			return ctx
		}, wantRoute: "tt-alert-stream"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			troopertest.New(t, "tt-alert-engine")
			var buf bytes.Buffer
			log := slog.New(EntryRoute(slog.NewJSONHandler(&buf, nil))).With("svc", "x")
			log.InfoContext(tt.ctx(), "hello")
			var rec map[string]any
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatal(err)
			}
			route, _ := rec["trooper.entry.route"].(string)
			method, _ := rec["trooper.entry.method"].(string)
			if route != tt.wantRoute || method != tt.wantMethod || rec["svc"] != "x" {
				t.Fatalf("record %v", rec)
			}
		})
	}
}

func TestEndRecordsStack(t *testing.T) {
	h := troopertest.New(t, "tt-alert-engine")
	_, span := StartSpan(context.Background(), "op")
	End(span, errors.New("boom"))
	s := h.SpanByName("op")
	var stack string
	for _, e := range s.Events() {
		for _, kv := range e.Attributes {
			if kv.Key == "exception.stacktrace" {
				stack = kv.Value.AsString()
			}
		}
	}
	if !strings.Contains(stack, "TestEndRecordsStack") {
		t.Fatalf("missing stack trace: %q", stack)
	}
}

func TestStartClient(t *testing.T) {
	h := troopertest.New(t, "tt-notifier")
	_, span := StartClient(context.Background(), "smtp send", "smtp", "smtp.example.com:587")
	End(span, nil)
	s := h.SpanByName("smtp send")
	if troopertest.AttrString(s, "peer.service") != "smtp" || troopertest.AttrString(s, "server.address") != "smtp.example.com" {
		t.Fatalf("attrs %v", s.Attributes())
	}
	if v, _ := troopertest.Attr(s, "server.port"); v.AsInt64() != 587 {
		t.Fatalf("port %v", v)
	}
}
