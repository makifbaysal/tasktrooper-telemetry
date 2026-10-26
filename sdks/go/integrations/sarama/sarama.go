package troopersarama

import (
	"context"
	"strconv"
	"strings"

	"github.com/IBM/sarama"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	trooper "github.com/tasktrooper/telemetry-go"
)

// Option configures consumer instrumentation.
type Option func(*config)

type config struct {
	group, process, attemptHeader string
	policy                        trooper.PanicPolicy
	onError                       func(context.Context, *sarama.ConsumerMessage, error) error
}

func newConfig(opts []Option) config {
	var c config
	for _, o := range opts {
		o(&c)
	}
	return c
}

// WithConsumerGroup sets messaging.consumer.group.name.
func WithConsumerGroup(group string) Option { return func(c *config) { c.group = group } }

// WithProcess sets trooper.process (and the entry route). Defaults to the group.
func WithProcess(name string) Option { return func(c *config) { c.process = name } }

// WithAttemptHeader reads trooper.consumer.attempt from an integer message header.
func WithAttemptHeader(name string) Option { return func(c *config) { c.attemptHeader = name } }

// WithPanicPolicy controls panics in handlers (default trooper.PanicRepanic).
func WithPanicPolicy(p trooper.PanicPolicy) Option { return func(c *config) { c.policy = p } }

// WithErrorHandler is called by NewConsumerGroupHandler when the handler fails. Returning nil marks
// the message and continues; returning an error stops ConsumeClaim with it. Default: mark and continue.
func WithErrorHandler(f func(context.Context, *sarama.ConsumerMessage, error) error) Option {
	return func(c *config) { c.onError = f }
}

// ProcessMessage runs fn inside a consumer entry span for msg, continuing the trace from its headers.
func ProcessMessage(ctx context.Context, msg *sarama.ConsumerMessage, fn func(context.Context) error, opts ...Option) error {
	return newConfig(opts).run(ctx, msg, fn)
}

func (c config) run(ctx context.Context, msg *sarama.ConsumerMessage, fn func(context.Context) error) error {
	if msg == nil {
		return fn(ctx)
	}
	carrier := ConsumerCarrier{Headers: &msg.Headers}
	m := trooper.Message{
		System:        "kafka",
		Destination:   msg.Topic,
		ConsumerGroup: c.group,
		Partition:     int(msg.Partition),
		Process:       c.process,
		Attempt:       attempt(carrier.Get(c.attemptHeader), c.attemptHeader),
		Attributes:    []attribute.KeyValue{attribute.Int64("messaging.kafka.offset", msg.Offset)},
	}
	return trooper.ProcessMessage(ctx, carrier, m, c.policy, fn)
}

func attempt(v, header string) int {
	if header == "" {
		return 1
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// HandlerFunc processes one message.
type HandlerFunc func(ctx context.Context, msg *sarama.ConsumerMessage) error

// NewConsumerGroupHandler returns a sarama.ConsumerGroupHandler that processes every claimed message
// with fn inside a consumer entry span and marks it afterwards (see WithErrorHandler).
func NewConsumerGroupHandler(fn HandlerFunc, opts ...Option) sarama.ConsumerGroupHandler {
	return &groupHandler{fn: fn, cfg: newConfig(opts)}
}

type groupHandler struct {
	fn  HandlerFunc
	cfg config
}

func (h *groupHandler) Setup(sarama.ConsumerGroupSession) error   { return nil }
func (h *groupHandler) Cleanup(sarama.ConsumerGroupSession) error { return nil }

func (h *groupHandler) ConsumeClaim(sess sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	ctx := sess.Context()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-claim.Messages():
			if !ok {
				return nil
			}
			err := h.cfg.run(ctx, msg, func(ctx context.Context) error { return h.fn(ctx, msg) })
			if err != nil && h.cfg.onError != nil {
				if stop := h.cfg.onError(ctx, msg, err); stop != nil {
					return stop
				}
			}
			sess.MarkMessage(msg, "")
		}
	}
}

// MessageSender is implemented by sarama.SyncProducer.
type MessageSender interface {
	SendMessage(msg *sarama.ProducerMessage) (partition int32, offset int64, err error)
}

// SendMessage sends msg inside a producer span and injects trace context + baggage into its headers.
func SendMessage(ctx context.Context, p MessageSender, msg *sarama.ProducerMessage) (partition int32, offset int64, err error) {
	_, span := trooper.StartProducer(ctx, ProducerCarrier{Headers: &msg.Headers},
		trooper.Message{System: "kafka", Destination: msg.Topic, Partition: -1})
	partition, offset, err = p.SendMessage(msg)
	if err == nil {
		span.SetAttributes(
			attribute.String("messaging.destination.partition.id", strconv.Itoa(int(partition))),
			attribute.Int64("messaging.kafka.offset", offset),
		)
	}
	trooper.EndSpan(span, err)
	return partition, offset, err
}

// InjectContext injects the trace context and baggage (tt.svc) of ctx into msg headers without
// creating a span.
func InjectContext(ctx context.Context, msg *sarama.ProducerMessage) {
	defer func() { _ = recover() }()
	otel.GetTextMapPropagator().Inject(ctx, ProducerCarrier{Headers: &msg.Headers})
}

// ProducerInterceptor injects caller baggage (tt.svc) into messages that carry no trace context
// yet. Register it in config.Producer.Interceptors.
type ProducerInterceptor struct{}

var _ sarama.ProducerInterceptor = ProducerInterceptor{}

// OnSend implements sarama.ProducerInterceptor.
func (ProducerInterceptor) OnSend(msg *sarama.ProducerMessage) {
	if msg == nil || (ProducerCarrier{Headers: &msg.Headers}).Get("traceparent") != "" {
		return
	}
	InjectContext(context.Background(), msg)
}

// ConsumerCarrier adapts consumer message headers to propagation.TextMapCarrier.
type ConsumerCarrier struct{ Headers *[]*sarama.RecordHeader }

func (c ConsumerCarrier) Get(key string) string {
	if c.Headers == nil || key == "" {
		return ""
	}
	for i := len(*c.Headers) - 1; i >= 0; i-- {
		if h := (*c.Headers)[i]; h != nil && string(h.Key) == key {
			return string(h.Value)
		}
	}
	return ""
}

func (c ConsumerCarrier) Set(key, value string) {
	if c.Headers == nil {
		return
	}
	for _, h := range *c.Headers {
		if h != nil && string(h.Key) == key {
			h.Value = []byte(value)
			return
		}
	}
	*c.Headers = append(*c.Headers, &sarama.RecordHeader{Key: []byte(key), Value: []byte(value)})
}

func (c ConsumerCarrier) Keys() []string {
	if c.Headers == nil {
		return nil
	}
	out := make([]string, 0, len(*c.Headers))
	for _, h := range *c.Headers {
		if h != nil {
			out = append(out, string(h.Key))
		}
	}
	return out
}

// ProducerCarrier adapts producer message headers to propagation.TextMapCarrier.
type ProducerCarrier struct{ Headers *[]sarama.RecordHeader }

func (c ProducerCarrier) Get(key string) string {
	if c.Headers == nil {
		return ""
	}
	for i := len(*c.Headers) - 1; i >= 0; i-- {
		if string((*c.Headers)[i].Key) == key {
			return string((*c.Headers)[i].Value)
		}
	}
	return ""
}

func (c ProducerCarrier) Set(key, value string) {
	if c.Headers == nil {
		return
	}
	for i := range *c.Headers {
		if string((*c.Headers)[i].Key) == key {
			(*c.Headers)[i].Value = []byte(value)
			return
		}
	}
	*c.Headers = append(*c.Headers, sarama.RecordHeader{Key: []byte(key), Value: []byte(value)})
}

func (c ProducerCarrier) Keys() []string {
	if c.Headers == nil {
		return nil
	}
	out := make([]string, 0, len(*c.Headers))
	for _, h := range *c.Headers {
		out = append(out, string(h.Key))
	}
	return out
}
