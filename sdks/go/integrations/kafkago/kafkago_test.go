package trooperkafkago

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

func msg(extra ...kafka.Header) kafka.Message {
	var hs []kafka.Header
	for k, v := range troopertest.CallerHeaders("upstream") {
		hs = append(hs, kafka.Header{Key: k, Value: []byte(v)})
	}
	return kafka.Message{Topic: "orders", Partition: 2, Offset: 5, Key: []byte("secret"), Headers: append(hs, extra...)}
}

func TestProcessMessage(t *testing.T) {
	tests := []struct {
		name               string
		opts               []Option
		hdr                []kafka.Header
		fn                 func() error
		wantErr, wantPanic bool
		proc, attempt      string
		status             codes.Code
	}{
		{"ok", []Option{WithConsumerGroup("g"), WithProcess("p")}, nil, func() error { return nil }, false, false, "p", "1", codes.Unset},
		{"attempt", []Option{WithConsumerGroup("g"), WithAttemptHeader("a")}, []kafka.Header{{Key: "a", Value: []byte("4")}}, func() error { return nil }, false, false, "g", "4", codes.Unset},
		{"error", []Option{WithConsumerGroup("g")}, nil, func() error { return errors.New("x") }, true, false, "g", "1", codes.Error},
		{"panic recover", []Option{WithConsumerGroup("g"), WithPanicPolicy(trooper.PanicRecover)}, nil, func() error { panic("x") }, true, false, "g", "1", codes.Error},
		{"panic repanic", []Option{WithConsumerGroup("g")}, nil, func() error { panic("x") }, false, true, "g", "1", codes.Error},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			var err error
			panicked := func() (p bool) {
				defer func() { p = recover() != nil }()
				err = ProcessMessage(context.Background(), msg(tt.hdr...), func(ctx context.Context) error {
					_, s := otel.Tracer("t").Start(ctx, "db", trace.WithSpanKind(trace.SpanKindClient))
					s.End()
					return tt.fn()
				}, tt.opts...)
				return
			}()
			if panicked != tt.wantPanic || (err != nil) != tt.wantErr {
				t.Fatalf("panicked=%v err=%v", panicked, err)
			}
			cs := h.SpanByKind(trace.SpanKindConsumer)
			if cs == nil || cs.Name() != "process orders" {
				t.Fatalf("span=%v", cs)
			}
			for k, v := range map[string]string{
				"messaging.system": "kafka", "messaging.destination.name": "orders", "messaging.consumer.group.name": "g",
				"messaging.destination.partition.id": "2", "messaging.kafka.offset": "5",
				"trooper.process": tt.proc, "trooper.entry.route": tt.proc, "trooper.consumer.attempt": tt.attempt,
				"trooper.caller": "upstream", "trooper.sample_rate": "1",
			} {
				if got := troopertest.AttrString(cs, attribute.Key(k)); got != v {
					t.Errorf("%s=%q want %q", k, got, v)
				}
			}
			if cs.Parent().TraceID().String() != "0102030405060708090a0b0c0d0e0f10" || cs.Status().Code != tt.status {
				t.Errorf("parent=%v status=%v", cs.Parent(), cs.Status())
			}
			if got := troopertest.AttrString(h.SpanByKind(trace.SpanKindClient), "trooper.entry.route"); got != tt.proc {
				t.Errorf("child entry route=%q", got)
			}
		})
	}
}

type fakeWriter struct {
	err  error
	msgs []kafka.Message
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	f.msgs = msgs
	return f.err
}

func TestWriteMessages(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{{"ok", nil}, {"error", errors.New("down")}} {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			ctx, parent := otel.Tracer("t").Start(context.Background(), "parent")
			w := &fakeWriter{err: tt.err}
			orig := []kafka.Header{{Key: "x", Value: []byte("1")}}
			err := WriteMessages(ctx, w, kafka.Message{Topic: "a", Headers: orig}, kafka.Message{Topic: "b"})
			parent.End()
			if !errors.Is(err, tt.err) {
				t.Fatal(err)
			}
			if len(orig) != 1 {
				t.Error("caller headers mutated")
			}
			for _, m := range w.msgs {
				c := Carrier{Headers: &m.Headers}
				if !strings.Contains(c.Get("traceparent"), parent.SpanContext().TraceID().String()) || !strings.Contains(c.Get("baggage"), "tt.svc=svc") {
					t.Errorf("headers=%v", c.Keys())
				}
			}
			n := 0
			for _, s := range h.Spans() {
				if s.SpanKind() == trace.SpanKindProducer {
					n++
					if (s.Status().Code == codes.Error) != (tt.err != nil) {
						t.Errorf("status=%v", s.Status())
					}
				}
			}
			if n != 2 {
				t.Errorf("producer spans=%d", n)
			}
		})
	}
}
