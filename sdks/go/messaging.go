package trooper

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Message describes a message for consumer/producer spans. Used by the messaging integrations
// (franz-go, sarama, amqp091, kafka-go); application code normally does not need it.
type Message struct {
	// System is messaging.system: "kafka", "rabbitmq", ...
	System string
	// Destination is the topic / queue / exchange (messaging.destination.name).
	Destination string
	// ConsumerGroup is messaging.consumer.group.name (consumers only).
	ConsumerGroup string
	// Partition is messaging.destination.partition.id; negative means unknown.
	Partition int
	// Process is trooper.process; defaults to ConsumerGroup, then Destination.
	Process string
	// Attempt is trooper.consumer.attempt (1 = first delivery); <= 0 means 1. If the carrier has a
	// tt-attempt header with a larger value, that value is used.
	Attempt int
	// Attributes are extra span attributes (e.g. messaging.kafka.offset). Keep them low-cardinality
	// except for IDs that only live on spans.
	Attributes []attribute.KeyValue
}

func (m Message) process() string { return first(m.Process, m.ConsumerGroup, m.Destination) }

func (m Message) baseAttrs() []attribute.KeyValue {
	kv := make([]attribute.KeyValue, 0, 8+len(m.Attributes))
	if m.System != "" {
		kv = append(kv, attribute.String("messaging.system", m.System))
	}
	if m.Destination != "" {
		kv = append(kv, attribute.String("messaging.destination.name", m.Destination))
	}
	if m.Partition >= 0 {
		kv = append(kv, attribute.String("messaging.destination.partition.id", strconv.Itoa(m.Partition)))
	}
	return append(kv, m.Attributes...)
}

func tracer() trace.Tracer {
	return otel.GetTracerProvider().Tracer(ScopeName, trace.WithInstrumentationVersion(Version))
}

// StartConsumer extracts the trace context and baggage from carrier and starts a consumer
// ("process <destination>") span as the entry span of the message: trooper.process,
// trooper.entry.route (= process), trooper.consumer.attempt and trooper.caller are set, and
// every span started from the returned context gets trooper.entry.route. End the span with EndSpan.
// While profile labels are enabled the goroutine keeps the pprof labels of this message until the
// next entry starts on it; prefer ProcessMessage, which restores them.
func StartConsumer(ctx context.Context, carrier propagation.TextMapCarrier, m Message) (context.Context, trace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	attempt := m.Attempt
	if carrier != nil {
		ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
		// Retry topics / re-publishes carry the attempt in the tt-attempt header (contract §4).
		if n, err := strconv.Atoi(strings.TrimSpace(carrier.Get(AttemptHeader))); err == nil && n > attempt {
			attempt = n
		}
	}
	if attempt <= 0 {
		attempt = 1
	}
	proc := m.process()
	kv := append(m.baseAttrs(),
		attribute.String("messaging.operation.type", "process"),
		AttrProcess.String(proc),
		AttrConsumerAttempt.Int(attempt),
	)
	if m.ConsumerGroup != "" {
		kv = append(kv, attribute.String("messaging.consumer.group.name", m.ConsumerGroup))
	}
	ctx, span := tracer().Start(ctx, "process "+first(m.Destination, proc),
		trace.WithSpanKind(trace.SpanKindConsumer), trace.WithAttributes(kv...))
	ctx, _ = BeginEntry(ctx, "", proc, nil)
	return ctx, span
}

// StartProducer starts a producer ("send <destination>") span and injects the trace context and
// baggage (with tt.svc) into carrier. End the span with EndSpan.
func StartProducer(ctx context.Context, carrier propagation.TextMapCarrier, m Message) (context.Context, trace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	kv := append(m.baseAttrs(), attribute.String("messaging.operation.type", "send"))
	ctx, span := tracer().Start(ctx, "send "+m.Destination,
		trace.WithSpanKind(trace.SpanKindProducer), trace.WithAttributes(kv...))
	if carrier != nil {
		otel.GetTextMapPropagator().Inject(ctx, carrier)
	}
	return ctx, span
}

// EndSpan records err (if any) and ends span.
func EndSpan(span trace.Span, err error) {
	if span == nil {
		return
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// ProcessMessage runs fn inside a consumer entry span (see StartConsumer). A panic in fn is
// recorded and, with PanicRepanic, re-raised after the span is ended; with PanicRecover it is
// returned as an error.
func ProcessMessage(ctx context.Context, carrier propagation.TextMapCarrier, m Message, policy PanicPolicy, fn func(context.Context) error) (err error) {
	ctx, span := StartConsumer(ctx, carrier, m)
	defer func() {
		EntryFromContext(ctx).restoreLabels()
		if r := recover(); r != nil {
			RecordPanic(ctx, r)
			span.End()
			if policy == PanicRepanic {
				panic(r)
			}
			err = &PanicError{Value: r}
			return
		}
		EndSpan(span, err)
	}()
	return fn(ctx)
}

// PanicError wraps a recovered panic value returned under PanicRecover.
type PanicError struct{ Value any }

func (e *PanicError) Error() string { return "panic: " + fmt.Sprint(e.Value) }

// AttemptHeader is the message header carrying the delivery attempt across retry topics and
// re-publishes (decimal string, .ai/contracts.md §4).
const AttemptHeader = "tt-attempt"

// SetAttempt writes the attempt number into carrier (use when re-publishing to a retry topic).
func SetAttempt(carrier propagation.TextMapCarrier, attempt int) {
	if carrier != nil && attempt > 0 {
		carrier.Set(AttemptHeader, strconv.Itoa(attempt))
	}
}
