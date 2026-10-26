package trooperkafkago

import (
	"context"
	"strconv"
	"strings"

	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
)

// Option configures ProcessMessage.
type Option func(*config)

type config struct {
	group, process, attemptHeader string
	policy                        trooper.PanicPolicy
}

// WithConsumerGroup sets messaging.consumer.group.name.
func WithConsumerGroup(g string) Option { return func(c *config) { c.group = g } }

// WithProcess sets trooper.process (and the entry route). Defaults to the group.
func WithProcess(name string) Option { return func(c *config) { c.process = name } }

// WithAttemptHeader reads trooper.consumer.attempt from an integer message header.
func WithAttemptHeader(name string) Option { return func(c *config) { c.attemptHeader = name } }

// WithPanicPolicy controls panics in fn (default trooper.PanicRepanic).
func WithPanicPolicy(p trooper.PanicPolicy) Option { return func(c *config) { c.policy = p } }

// ProcessMessage runs fn inside a consumer entry span for msg, continuing the trace from its headers.
func ProcessMessage(ctx context.Context, msg kafka.Message, fn func(context.Context) error, opts ...Option) error {
	var c config
	for _, o := range opts {
		o(&c)
	}
	carrier := Carrier{Headers: &msg.Headers}
	attempt := 1
	if c.attemptHeader != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(carrier.Get(c.attemptHeader))); err == nil && n > 0 {
			attempt = n
		}
	}
	return trooper.ProcessMessage(ctx, carrier, trooper.Message{
		System:        "kafka",
		Destination:   msg.Topic,
		ConsumerGroup: c.group,
		Partition:     msg.Partition,
		Process:       c.process,
		Attempt:       attempt,
		Attributes:    []attribute.KeyValue{attribute.Int64("messaging.kafka.offset", msg.Offset)},
	}, c.policy, fn)
}

// MessageWriter is implemented by *kafka.Writer.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

var _ MessageWriter = (*kafka.Writer)(nil)

// WriteMessages writes msgs with one producer span per message and injects the trace context and
// baggage (tt.svc) into each message's headers. The topic comes from the message or, for
// *kafka.Writer, from Writer.Topic.
func WriteMessages(ctx context.Context, w MessageWriter, msgs ...kafka.Message) error {
	defaultTopic := ""
	if kw, ok := w.(*kafka.Writer); ok && kw != nil {
		defaultTopic = kw.Topic
	}
	spans := make([]trace.Span, len(msgs))
	for i := range msgs {
		// copy headers so the caller's slice is not mutated
		msgs[i].Headers = append([]kafka.Header(nil), msgs[i].Headers...)
		topic := msgs[i].Topic
		if topic == "" {
			topic = defaultTopic
		}
		_, spans[i] = trooper.StartProducer(ctx, Carrier{Headers: &msgs[i].Headers},
			trooper.Message{System: "kafka", Destination: topic, Partition: -1})
	}
	err := w.WriteMessages(ctx, msgs...)
	for _, s := range spans {
		trooper.EndSpan(s, err)
	}
	return err
}

// Carrier adapts kafka-go headers to propagation.TextMapCarrier.
type Carrier struct{ Headers *[]kafka.Header }

func (c Carrier) Get(key string) string {
	if c.Headers == nil {
		return ""
	}
	for i := len(*c.Headers) - 1; i >= 0; i-- {
		if (*c.Headers)[i].Key == key {
			return string((*c.Headers)[i].Value)
		}
	}
	return ""
}

func (c Carrier) Set(key, value string) {
	if c.Headers == nil {
		return
	}
	for i := range *c.Headers {
		if (*c.Headers)[i].Key == key {
			(*c.Headers)[i].Value = []byte(value)
			return
		}
	}
	*c.Headers = append(*c.Headers, kafka.Header{Key: key, Value: []byte(value)})
}

func (c Carrier) Keys() []string {
	if c.Headers == nil {
		return nil
	}
	out := make([]string, 0, len(*c.Headers))
	for _, h := range *c.Headers {
		out = append(out, h.Key)
	}
	return out
}
