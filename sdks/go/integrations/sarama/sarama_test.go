package troopersarama

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/IBM/sarama"
	"github.com/IBM/sarama/mocks"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

func newMsg(extra ...*sarama.RecordHeader) *sarama.ConsumerMessage {
	var hs []*sarama.RecordHeader
	for k, v := range troopertest.CallerHeaders("upstream") {
		hs = append(hs, &sarama.RecordHeader{Key: []byte(k), Value: []byte(v)})
	}
	return &sarama.ConsumerMessage{Topic: "payments", Partition: 1, Offset: 9, Key: []byte("secret"), Headers: append(hs, extra...)}
}

func childFn(fn func() error) func(context.Context) error {
	return func(ctx context.Context) error {
		_, s := otel.Tracer("t").Start(ctx, "db", trace.WithSpanKind(trace.SpanKindClient))
		s.End()
		return fn()
	}
}

func TestProcessMessage(t *testing.T) {
	tests := []struct {
		name                string
		opts                []Option
		hdr                 []*sarama.RecordHeader
		fn                  func() error
		wantErr, wantPanic  bool
		wantProcess, wantAt string
		wantStatus          codes.Code
	}{
		{"ok", []Option{WithConsumerGroup("pay"), WithProcess("pay-worker")}, nil, func() error { return nil }, false, false, "pay-worker", "1", codes.Unset},
		{"attempt", []Option{WithConsumerGroup("pay"), WithAttemptHeader("retry")}, []*sarama.RecordHeader{{Key: []byte("retry"), Value: []byte("2")}}, func() error { return nil }, false, false, "pay", "2", codes.Unset},
		{"error", []Option{WithConsumerGroup("pay")}, nil, func() error { return errors.New("x") }, true, false, "pay", "1", codes.Error},
		{"panic recover", []Option{WithConsumerGroup("pay"), WithPanicPolicy(trooper.PanicRecover)}, nil, func() error { panic("p") }, true, false, "pay", "1", codes.Error},
		{"panic repanic", []Option{WithConsumerGroup("pay")}, nil, func() error { panic("p") }, false, true, "pay", "1", codes.Error},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			var err error
			panicked := func() (p bool) {
				defer func() { p = recover() != nil }()
				err = ProcessMessage(context.Background(), newMsg(tt.hdr...), childFn(tt.fn), tt.opts...)
				return
			}()
			if panicked != tt.wantPanic || (err != nil) != tt.wantErr {
				t.Fatalf("panicked=%v err=%v", panicked, err)
			}
			cs := h.SpanByKind(trace.SpanKindConsumer)
			if cs == nil || cs.Name() != "process payments" {
				t.Fatalf("consumer span=%v", cs)
			}
			for k, v := range map[string]string{
				"messaging.system": "kafka", "messaging.destination.name": "payments", "messaging.consumer.group.name": "pay",
				"messaging.destination.partition.id": "1", "messaging.kafka.offset": "9",
				"trooper.process": tt.wantProcess, "trooper.entry.route": tt.wantProcess, "trooper.consumer.attempt": tt.wantAt,
				"trooper.caller": "upstream", "trooper.sample_rate": "1",
			} {
				if got := troopertest.AttrString(cs, attribute.Key(k)); got != v {
					t.Errorf("%s=%q want %q", k, got, v)
				}
			}
			if cs.Parent().TraceID().String() != "0102030405060708090a0b0c0d0e0f10" {
				t.Error("trace not continued")
			}
			if cs.Status().Code != tt.wantStatus {
				t.Errorf("status=%v", cs.Status())
			}
			if got := troopertest.AttrString(h.SpanByKind(trace.SpanKindClient), "trooper.entry.route"); got != tt.wantProcess {
				t.Errorf("child entry route=%q", got)
			}
		})
	}
}

type fakeSession struct {
	sarama.ConsumerGroupSession
	ctx    context.Context
	marked []int64
}

func (s *fakeSession) Context() context.Context { return s.ctx }
func (s *fakeSession) MarkMessage(m *sarama.ConsumerMessage, _ string) {
	s.marked = append(s.marked, m.Offset)
}

type fakeClaim struct {
	sarama.ConsumerGroupClaim
	ch chan *sarama.ConsumerMessage
}

func (c fakeClaim) Messages() <-chan *sarama.ConsumerMessage { return c.ch }

func TestConsumerGroupHandler(t *testing.T) {
	stop := errors.New("stop")
	tests := []struct {
		name       string
		opts       []Option
		wantErr    error
		wantMarked int
	}{
		{"marks all by default", nil, nil, 2},
		{"error handler stops", []Option{WithErrorHandler(func(context.Context, *sarama.ConsumerMessage, error) error { return stop })}, stop, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			ch := make(chan *sarama.ConsumerMessage, 3)
			m1, m2 := newMsg(), newMsg()
			m2.Offset = 10
			ch <- m1
			ch <- m2
			close(ch)
			handler := NewConsumerGroupHandler(func(_ context.Context, m *sarama.ConsumerMessage) error {
				if m.Offset == 10 {
					return errors.New("bad")
				}
				return nil
			}, append(tt.opts, WithConsumerGroup("pay"))...)
			sess := &fakeSession{ctx: context.Background()}
			if err := handler.ConsumeClaim(sess, fakeClaim{ch: ch}); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err=%v", err)
			}
			if len(sess.marked) != tt.wantMarked {
				t.Errorf("marked=%v", sess.marked)
			}
			if n := len(h.Spans()); n != 2 {
				t.Errorf("spans=%d", n)
			}
		})
	}
}

func TestSendMessage(t *testing.T) {
	for _, tt := range []struct {
		name    string
		fail    error
		wantErr bool
	}{{"ok", nil, false}, {"fail", errors.New("down"), true}} {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			p := mocks.NewSyncProducer(t, nil)
			if tt.fail != nil {
				p.ExpectSendMessageAndFail(tt.fail)
			} else {
				p.ExpectSendMessageAndSucceed()
			}
			ctx, parent := otel.Tracer("t").Start(context.Background(), "parent")
			msg := &sarama.ProducerMessage{Topic: "payments", Value: sarama.StringEncoder("secret")}
			_, _, err := SendMessage(ctx, p, msg)
			parent.End()
			if (err != nil) != tt.wantErr {
				t.Fatal(err)
			}
			c := ProducerCarrier{Headers: &msg.Headers}
			if !strings.Contains(c.Get("traceparent"), parent.SpanContext().TraceID().String()) || !strings.Contains(c.Get("baggage"), "tt.svc=svc") {
				t.Errorf("headers=%v", c.Keys())
			}
			ps := h.SpanByKind(trace.SpanKindProducer)
			if ps == nil || ps.Name() != "send payments" || (ps.Status().Code == codes.Error) != tt.wantErr {
				t.Errorf("producer span=%v", ps)
			}
			_ = p.Close()
		})
	}
}

func TestProducerInterceptor(t *testing.T) {
	troopertest.New(t, "svc")
	msg := &sarama.ProducerMessage{Topic: "x"}
	ProducerInterceptor{}.OnSend(msg)
	c := ProducerCarrier{Headers: &msg.Headers}
	if !strings.Contains(c.Get("baggage"), "tt.svc=svc") {
		t.Errorf("baggage=%q", c.Get("baggage"))
	}
	// existing trace context is left untouched
	msg2 := &sarama.ProducerMessage{Topic: "x", Headers: []sarama.RecordHeader{{Key: []byte("traceparent"), Value: []byte("keep")}}}
	ProducerInterceptor{}.OnSend(msg2)
	if len(msg2.Headers) != 1 {
		t.Errorf("headers modified: %v", msg2.Headers)
	}
	ProducerInterceptor{}.OnSend(nil)
}
