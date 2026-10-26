package trooperfranz

import (
	"context"
	"strconv"
	"strings"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
)

// Option configures a Tracer.
type Option func(*Tracer)

// WithConsumerGroup sets messaging.consumer.group.name on consumer spans.
func WithConsumerGroup(group string) Option { return func(t *Tracer) { t.group = group } }

// WithProcess sets trooper.process (and the entry route) of consumer spans. Defaults to the group.
func WithProcess(name string) Option { return func(t *Tracer) { t.process = name } }

// WithAttemptHeader reads trooper.consumer.attempt from the given record header (integer;
// missing or invalid means 1).
func WithAttemptHeader(name string) Option { return func(t *Tracer) { t.attemptHeader = name } }

// WithPanicPolicy controls panics in ProcessRecord callbacks (default trooper.PanicRepanic).
func WithPanicPolicy(p trooper.PanicPolicy) Option { return func(t *Tracer) { t.policy = p } }

// Tracer instruments a kgo client.
type Tracer struct {
	group, process, attemptHeader string
	policy                        trooper.PanicPolicy
}

// New returns a Tracer.
func New(opts ...Option) *Tracer {
	t := &Tracer{}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Hooks returns kgo hooks that create producer spans and inject trace headers. Pass them with
// kgo.WithHooks.
func (t *Tracer) Hooks() []kgo.Hook { return []kgo.Hook{producerHooks{}} }

// ProcessRecord runs fn inside a consumer entry span for rec. The span continues the trace from
// the record headers. The error returned by fn is recorded and returned.
func (t *Tracer) ProcessRecord(ctx context.Context, rec *kgo.Record, fn func(context.Context) error) error {
	if rec == nil {
		return fn(ctx)
	}
	m := trooper.Message{
		System:        "kafka",
		Destination:   rec.Topic,
		ConsumerGroup: t.group,
		Partition:     int(rec.Partition),
		Process:       t.process,
		Attempt:       t.attempt(rec),
		Attributes:    []attribute.KeyValue{attribute.Int64("messaging.kafka.offset", rec.Offset)},
	}
	return trooper.ProcessMessage(ctx, Carrier{Headers: &rec.Headers}, m, t.policy, fn)
}

func (t *Tracer) attempt(rec *kgo.Record) int {
	if t.attemptHeader == "" {
		return 1
	}
	v := Carrier{Headers: &rec.Headers}.Get(t.attemptHeader)
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

type spanKey struct{}

type producerHooks struct{}

var (
	_ kgo.HookProduceRecordBuffered   = producerHooks{}
	_ kgo.HookProduceRecordUnbuffered = producerHooks{}
)

func (producerHooks) OnProduceRecordBuffered(r *kgo.Record) {
	defer func() { _ = recover() }()
	ctx := r.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, span := trooper.StartProducer(ctx, Carrier{Headers: &r.Headers}, trooper.Message{
		System: "kafka", Destination: r.Topic, Partition: -1,
	})
	r.Context = context.WithValue(ctx, spanKey{}, span)
}

func (producerHooks) OnProduceRecordUnbuffered(r *kgo.Record, err error) {
	defer func() { _ = recover() }()
	if r.Context == nil {
		return
	}
	span, ok := r.Context.Value(spanKey{}).(trace.Span)
	if !ok {
		return
	}
	if err == nil {
		span.SetAttributes(
			attribute.String("messaging.destination.partition.id", strconv.Itoa(int(r.Partition))),
			attribute.Int64("messaging.kafka.offset", r.Offset),
		)
	}
	trooper.EndSpan(span, err)
}

// Carrier adapts kgo record headers to propagation.TextMapCarrier.
type Carrier struct{ Headers *[]kgo.RecordHeader }

// Get returns the value of the last header named key.
func (c Carrier) Get(key string) string {
	if c.Headers == nil {
		return ""
	}
	h := *c.Headers
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Key == key {
			return string(h[i].Value)
		}
	}
	return ""
}

// Set replaces header key or appends it.
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
	*c.Headers = append(*c.Headers, kgo.RecordHeader{Key: key, Value: []byte(value)})
}

// Keys lists header names.
func (c Carrier) Keys() []string {
	if c.Headers == nil {
		return nil
	}
	keys := make([]string, 0, len(*c.Headers))
	for _, h := range *c.Headers {
		keys = append(keys, h.Key)
	}
	return keys
}
