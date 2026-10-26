package trooperamqp

import (
	"context"
	"errors"
	"strings"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

func upstream() amqp.Table {
	t := amqp.Table{}
	for k, v := range troopertest.CallerHeaders("upstream") {
		t[k] = v
	}
	return t
}

func TestAttempt(t *testing.T) {
	tests := []struct {
		name string
		d    amqp.Delivery
		q    string
		want int
	}{
		{"first", amqp.Delivery{}, "q", 1},
		{"redelivered", amqp.Delivery{Redelivered: true}, "q", 2},
		{"x-death matching queue", amqp.Delivery{Headers: amqp.Table{"x-death": []interface{}{
			amqp.Table{"queue": "retry", "count": int64(5)}, amqp.Table{"queue": "q", "count": int64(2)}}}}, "q", 3},
		{"x-death first entry", amqp.Delivery{Headers: amqp.Table{"x-death": []interface{}{amqp.Table{"queue": "other", "count": int64(1)}}}}, "q", 2},
		{"x-death garbage", amqp.Delivery{Headers: amqp.Table{"x-death": "nope"}}, "q", 1},
		{"x-delivery-count quorum", amqp.Delivery{Headers: amqp.Table{"x-delivery-count": int64(2)}, Redelivered: true}, "q", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Attempt(tt.d, tt.q); got != tt.want {
				t.Errorf("got %d want %d", got, tt.want)
			}
		})
	}
}

func TestProcessDelivery(t *testing.T) {
	tests := []struct {
		name               string
		opts               []Option
		d                  amqp.Delivery
		fn                 func(ctx context.Context) error
		wantErr, wantPanic bool
		wantDest, wantProc string
		wantAttempt        string
		wantDLQ            bool
		wantStatus         codes.Code
	}{
		{name: "ok", opts: []Option{WithQueue("orders"), WithProcess("orders-worker")}, d: amqp.Delivery{RoutingKey: "orders.created"},
			fn: func(context.Context) error { return nil }, wantDest: "orders", wantProc: "orders-worker", wantAttempt: "1"},
		{name: "queue from routing key", d: amqp.Delivery{RoutingKey: "orders", Redelivered: true},
			fn: func(context.Context) error { return nil }, wantDest: "orders", wantProc: "orders", wantAttempt: "2"},
		{name: "dlq", opts: []Option{WithQueue("orders")}, d: amqp.Delivery{},
			fn:      func(ctx context.Context) error { trooper.MarkDLQ(ctx); return errors.New("poison") },
			wantErr: true, wantDest: "orders", wantProc: "orders", wantAttempt: "1", wantDLQ: true, wantStatus: codes.Error},
		{name: "panic recover", opts: []Option{WithQueue("orders"), WithPanicPolicy(trooper.PanicRecover)},
			fn:      func(context.Context) error { panic("x") },
			wantErr: true, wantDest: "orders", wantProc: "orders", wantAttempt: "1", wantStatus: codes.Error},
		{name: "panic repanic", opts: []Option{WithQueue("orders")},
			fn:        func(context.Context) error { panic("x") },
			wantPanic: true, wantDest: "orders", wantProc: "orders", wantAttempt: "1", wantStatus: codes.Error},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			d := tt.d
			d.Body = []byte("secret")
			hdr := upstream()
			for k, v := range d.Headers {
				hdr[k] = v
			}
			d.Headers = hdr
			var err error
			panicked := func() (p bool) {
				defer func() { p = recover() != nil }()
				err = ProcessDelivery(context.Background(), d, func(ctx context.Context) error {
					_, s := otel.Tracer("t").Start(ctx, "db", trace.WithSpanKind(trace.SpanKindClient))
					s.End()
					return tt.fn(ctx)
				}, tt.opts...)
				return
			}()
			if panicked != tt.wantPanic || (err != nil) != tt.wantErr {
				t.Fatalf("panicked=%v err=%v", panicked, err)
			}
			cs := h.SpanByKind(trace.SpanKindConsumer)
			if cs == nil || cs.Name() != "process "+tt.wantDest {
				t.Fatalf("consumer span=%v", cs)
			}
			for k, v := range map[string]string{
				"messaging.system": "rabbitmq", "messaging.destination.name": tt.wantDest,
				"trooper.process": tt.wantProc, "trooper.entry.route": tt.wantProc,
				"trooper.consumer.attempt": tt.wantAttempt, "trooper.caller": "upstream", "trooper.sample_rate": "1",
			} {
				if got := troopertest.AttrString(cs, attribute.Key(k)); got != v {
					t.Errorf("%s=%q want %q", k, got, v)
				}
			}
			if _, ok := troopertest.Attr(cs, "messaging.destination.partition.id"); ok {
				t.Error("rabbitmq span must not have partition")
			}
			_, dlq := troopertest.Attr(cs, trooper.AttrConsumerDLQ)
			if dlq != tt.wantDLQ {
				t.Errorf("dlq=%v", dlq)
			}
			if cs.Parent().TraceID().String() != "0102030405060708090a0b0c0d0e0f10" || cs.Status().Code != tt.wantStatus {
				t.Errorf("parent=%v status=%v", cs.Parent(), cs.Status())
			}
			if got := troopertest.AttrString(h.SpanByKind(trace.SpanKindClient), "trooper.entry.route"); got != tt.wantProc {
				t.Errorf("child entry route=%q", got)
			}
		})
	}
}

type fakePub struct {
	err  error
	got  amqp.Publishing
	dest string
}

func (f *fakePub) PublishWithContext(_ context.Context, exchange, key string, _, _ bool, msg amqp.Publishing) error {
	f.got, f.dest = msg, exchange+"|"+key
	return f.err
}

func TestPublish(t *testing.T) {
	tests := []struct {
		name, exchange, key, wantDest string
		err                           error
	}{
		{"exchange", "events", "orders.created", "events", nil},
		{"default exchange", "", "orders", "orders", nil},
		{"error", "events", "k", "events", errors.New("closed")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			ctx, parent := otel.Tracer("t").Start(context.Background(), "parent")
			orig := amqp.Table{"x-custom": "1"}
			p := &fakePub{err: tt.err}
			err := Publish(ctx, p, tt.exchange, tt.key, false, false, amqp.Publishing{Headers: orig, Body: []byte("secret")})
			parent.End()
			if !errors.Is(err, tt.err) {
				t.Fatal(err)
			}
			c := Carrier(p.got.Headers)
			if !strings.Contains(c.Get("traceparent"), parent.SpanContext().TraceID().String()) || !strings.Contains(c.Get("baggage"), "tt.svc=svc") || c.Get("x-custom") != "1" {
				t.Errorf("headers=%v", p.got.Headers)
			}
			if len(orig) != 1 {
				t.Error("caller headers mutated")
			}
			ps := h.SpanByKind(trace.SpanKindProducer)
			if ps == nil || ps.Name() != "send "+tt.wantDest || (ps.Status().Code == codes.Error) != (tt.err != nil) {
				t.Errorf("producer span=%v", ps)
			}
		})
	}
}
