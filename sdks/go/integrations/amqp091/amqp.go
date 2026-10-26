package trooperamqp

import (
	"context"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel/attribute"

	trooper "github.com/tasktrooper/telemetry-go"
)

// Publisher is implemented by *amqp.Channel.
type Publisher interface {
	PublishWithContext(ctx context.Context, exchange, key string, mandatory, immediate bool, msg amqp.Publishing) error
}

var _ Publisher = (*amqp.Channel)(nil)

// Publish publishes msg inside a producer span and injects the trace context and baggage into
// msg.Headers (a copy of the headers table is made; the caller's table is not mutated).
func Publish(ctx context.Context, ch Publisher, exchange, key string, mandatory, immediate bool, msg amqp.Publishing) error {
	headers := amqp.Table{}
	for k, v := range msg.Headers {
		headers[k] = v
	}
	msg.Headers = headers
	dest := exchange
	if dest == "" {
		dest = key // default exchange routes by queue name
	}
	attrs := []attribute.KeyValue{}
	if key != "" {
		attrs = append(attrs, attribute.String("messaging.rabbitmq.destination.routing_key", key))
	}
	ctx, span := trooper.StartProducer(ctx, Carrier(headers), trooper.Message{
		System: "rabbitmq", Destination: dest, Partition: -1, Attributes: attrs,
	})
	err := ch.PublishWithContext(ctx, exchange, key, mandatory, immediate, msg)
	trooper.EndSpan(span, err)
	return err
}

// Option configures ProcessDelivery.
type Option func(*config)

type config struct {
	queue, process string
	policy         trooper.PanicPolicy
}

// WithQueue sets the consumed queue (messaging.destination.name). Defaults to the routing key.
func WithQueue(q string) Option { return func(c *config) { c.queue = q } }

// WithProcess sets trooper.process (and the entry route). Defaults to the queue.
func WithProcess(name string) Option { return func(c *config) { c.process = name } }

// WithPanicPolicy controls panics in fn (default trooper.PanicRepanic).
func WithPanicPolicy(p trooper.PanicPolicy) Option { return func(c *config) { c.policy = p } }

// ProcessDelivery runs fn inside a consumer entry span for d. Acking/nacking is left to fn.
// Use trooper.MarkDLQ(ctx) inside fn when the message is dead-lettered.
func ProcessDelivery(ctx context.Context, d amqp.Delivery, fn func(context.Context) error, opts ...Option) error {
	var c config
	for _, o := range opts {
		o(&c)
	}
	queue := c.queue
	if queue == "" {
		queue = d.RoutingKey
	}
	attrs := []attribute.KeyValue{}
	if d.RoutingKey != "" {
		attrs = append(attrs, attribute.String("messaging.rabbitmq.destination.routing_key", d.RoutingKey))
	}
	if d.MessageId != "" {
		attrs = append(attrs, attribute.String("messaging.message.id", d.MessageId))
	}
	m := trooper.Message{
		System:      "rabbitmq",
		Destination: queue,
		Partition:   -1,
		Process:     c.process,
		Attempt:     Attempt(d, queue),
		Attributes:  attrs,
	}
	return trooper.ProcessMessage(ctx, Carrier(d.Headers), m, c.policy, fn)
}

// Attempt derives the delivery attempt: x-delivery-count + 1 (quorum queues); else x-death count for queue (or the first x-death entry) + 1;
// 2 if the delivery is a redelivery without x-death; otherwise 1.
func Attempt(d amqp.Delivery, queue string) int {
	// Quorum queues: x-delivery-count = number of previous delivery attempts.
	if n := toInt64(d.Headers["x-delivery-count"]); n > 0 {
		return int(n) + 1
	}
	if deaths, ok := d.Headers["x-death"].([]interface{}); ok && len(deaths) > 0 {
		count, found := int64(0), false
		for _, e := range deaths {
			t, ok := e.(amqp.Table)
			if !ok {
				continue
			}
			n := toInt64(t["count"])
			if q, _ := t["queue"].(string); q == queue && queue != "" {
				count, found = n, true
				break
			}
			if !found && count == 0 {
				count = n
			}
		}
		if count > 0 {
			return int(count) + 1
		}
	}
	if d.Redelivered {
		return 2
	}
	return 1
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	case int16:
		return int64(n)
	case int8:
		return int64(n)
	case uint8:
		return int64(n)
	case uint16:
		return int64(n)
	case uint32:
		return int64(n)
	}
	return 0
}

// Carrier adapts amqp.Table headers to propagation.TextMapCarrier. Set on a nil table is a no-op.
type Carrier amqp.Table

func (c Carrier) Get(key string) string {
	switch v := c[key].(type) {
	case nil:
		return ""
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}

func (c Carrier) Set(key, value string) {
	if c != nil {
		c[key] = value
	}
}

func (c Carrier) Keys() []string {
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	return out
}
