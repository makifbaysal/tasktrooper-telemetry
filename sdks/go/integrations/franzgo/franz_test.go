package trooperfranz

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

func upstreamHeaders() []kgo.RecordHeader {
	var hs []kgo.RecordHeader
	for k, v := range troopertest.CallerHeaders("upstream") {
		hs = append(hs, kgo.RecordHeader{Key: k, Value: []byte(v)})
	}
	return hs
}

func TestProcessRecord(t *testing.T) {
	errBoom := errors.New("boom")
	tests := []struct {
		name        string
		opts        []Option
		extraHdr    []kgo.RecordHeader
		fn          func(context.Context) error
		wantErr     bool
		wantPanic   bool
		wantAttempt string
		wantProcess string
		wantStatus  codes.Code
	}{
		{name: "ok", opts: []Option{WithConsumerGroup("g1"), WithProcess("orders-worker")},
			fn: func(context.Context) error { return nil }, wantAttempt: "1", wantProcess: "orders-worker", wantStatus: codes.Unset},
		{name: "process defaults to group", opts: []Option{WithConsumerGroup("g1")},
			fn: func(context.Context) error { return nil }, wantAttempt: "1", wantProcess: "g1"},
		{name: "attempt header", opts: []Option{WithConsumerGroup("g1"), WithAttemptHeader("x-attempt")},
			extraHdr: []kgo.RecordHeader{{Key: "x-attempt", Value: []byte("3")}},
			fn:       func(context.Context) error { return nil }, wantAttempt: "3", wantProcess: "g1"},
		{name: "error", opts: []Option{WithConsumerGroup("g1")},
			fn: func(context.Context) error { return errBoom }, wantErr: true, wantAttempt: "1", wantProcess: "g1", wantStatus: codes.Error},
		{name: "panic recover", opts: []Option{WithConsumerGroup("g1"), WithPanicPolicy(trooper.PanicRecover)},
			fn: func(context.Context) error { panic("bad") }, wantErr: true, wantAttempt: "1", wantProcess: "g1", wantStatus: codes.Error},
		{name: "panic repanic", opts: []Option{WithConsumerGroup("g1")},
			fn: func(context.Context) error { panic("bad") }, wantPanic: true, wantAttempt: "1", wantProcess: "g1", wantStatus: codes.Error},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			rec := &kgo.Record{Topic: "orders.created", Partition: 4, Offset: 42, Key: []byte("secret"),
				Headers: append(upstreamHeaders(), tt.extraHdr...)}
			tr := New(tt.opts...)
			var err error
			panicked := func() (p bool) {
				defer func() { p = recover() != nil }()
				err = tr.ProcessRecord(context.Background(), rec, func(ctx context.Context) error {
					_, child := otel.Tracer("test").Start(ctx, "db", trace.WithSpanKind(trace.SpanKindClient))
					child.End()
					return tt.fn(ctx)
				})
				return false
			}()
			if panicked != tt.wantPanic {
				t.Fatalf("panicked=%v want %v", panicked, tt.wantPanic)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			cs := h.SpanByKind(trace.SpanKindConsumer)
			if cs == nil {
				t.Fatal("no consumer span")
			}
			if cs.Name() != "process orders.created" {
				t.Errorf("name=%q", cs.Name())
			}
			want := map[string]string{
				"messaging.system":                   "kafka",
				"messaging.destination.name":         "orders.created",
				"messaging.consumer.group.name":      "g1",
				"messaging.destination.partition.id": "4",
				"messaging.kafka.offset":             "42",
				"trooper.process":                    tt.wantProcess,
				"trooper.entry.route":                tt.wantProcess,
				"trooper.consumer.attempt":           tt.wantAttempt,
				"trooper.caller":                     "upstream",
				"trooper.sample_rate":                "1",
			}
			for k, v := range want {
				if got := troopertest.AttrString(cs, attrKey(k)); got != v {
					t.Errorf("%s=%q want %q", k, got, v)
				}
			}
			if cs.Parent().TraceID().String() != "0102030405060708090a0b0c0d0e0f10" || !cs.Parent().IsRemote() {
				t.Errorf("trace not continued: parent=%v", cs.Parent())
			}
			if cs.Status().Code != tt.wantStatus {
				t.Errorf("status=%v want %v", cs.Status().Code, tt.wantStatus)
			}
			for _, kv := range cs.Attributes() {
				if strings.Contains(kv.Value.Emit(), "secret") {
					t.Errorf("record key leaked in %s", kv.Key)
				}
			}
			child := h.SpanByKind(trace.SpanKindClient)
			if got := troopertest.AttrString(child, "trooper.entry.route"); got != tt.wantProcess {
				t.Errorf("child entry route=%q want %q", got, tt.wantProcess)
			}
			if _, ok := troopertest.Attr(child, "trooper.sample_rate"); ok {
				t.Error("child span must not carry sample rate")
			}
		})
	}
}

func TestProducerHooks(t *testing.T) {
	for _, tt := range []struct {
		name    string
		err     error
		wantErr bool
	}{{"ok", nil, false}, {"error", errors.New("broker down"), true}} {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			ctx, parent := otel.Tracer("test").Start(context.Background(), "handler")
			ctx, _ = trooper.BeginEntry(ctx, "POST", "/orders", nil)
			rec := &kgo.Record{Topic: "orders.created", Context: ctx}
			for _, hk := range New().Hooks() {
				if b, ok := hk.(kgo.HookProduceRecordBuffered); ok {
					b.OnProduceRecordBuffered(rec)
				}
			}
			rec.Partition, rec.Offset = 2, 7
			for _, hk := range New().Hooks() {
				if u, ok := hk.(kgo.HookProduceRecordUnbuffered); ok {
					u.OnProduceRecordUnbuffered(rec, tt.err)
				}
			}
			parent.End()
			c := Carrier{Headers: &rec.Headers}
			if !strings.HasPrefix(c.Get("traceparent"), "00-"+parent.SpanContext().TraceID().String()) {
				t.Errorf("traceparent=%q", c.Get("traceparent"))
			}
			if !strings.Contains(c.Get("baggage"), "tt.svc=svc") {
				t.Errorf("baggage=%q", c.Get("baggage"))
			}
			ps := h.SpanByKind(trace.SpanKindProducer)
			if ps == nil || ps.Name() != "send orders.created" {
				t.Fatalf("producer span=%v", ps)
			}
			if got := troopertest.AttrString(ps, "trooper.entry.route"); got != "/orders" {
				t.Errorf("producer entry route=%q", got)
			}
			if (ps.Status().Code == codes.Error) != tt.wantErr {
				t.Errorf("status=%v", ps.Status())
			}
			if !tt.wantErr && troopertest.AttrString(ps, "messaging.kafka.offset") != "7" {
				t.Error("offset missing")
			}
		})
	}
}

func TestUnbufferedWithoutBufferedIsNoop(t *testing.T) {
	h := troopertest.New(t, "svc")
	ctx, user := otel.Tracer("test").Start(context.Background(), "user")
	producerHooks{}.OnProduceRecordUnbuffered(&kgo.Record{Context: ctx}, nil)
	producerHooks{}.OnProduceRecordUnbuffered(&kgo.Record{}, nil)
	if len(h.Spans()) != 0 || !user.IsRecording() {
		t.Fatal("must not end foreign spans")
	}
	user.End()
}
