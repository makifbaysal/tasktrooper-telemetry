// Package trooperamqp instruments github.com/rabbitmq/amqp091-go for TaskTrooper.
//
// Producer: Publish wraps Channel.PublishWithContext with a producer span and injects the trace
// context + baggage (tt.svc) into Publishing.Headers.
//
// Consumer: wrap each delivery with ProcessDelivery. It continues the trace from the headers and
// runs fn inside a consumer entry span (messaging.system=rabbitmq, trooper.process,
// trooper.consumer.attempt derived from x-death / Redelivered, trooper.caller).
//
//	for d := range deliveries {
//		err := trooperamqp.ProcessDelivery(ctx, d, func(ctx context.Context) error {
//			if err := handle(ctx, d); err != nil {
//				if permanent(err) {
//					trooper.MarkDLQ(ctx) // trooper.consumer.dlq=true
//					return d.Nack(false, false)
//				}
//				return err
//			}
//			return d.Ack(false)
//		}, trooperamqp.WithQueue("orders"), trooperamqp.WithProcess("orders-worker"))
//	}
//
// Message bodies are never recorded.
package trooperamqp
