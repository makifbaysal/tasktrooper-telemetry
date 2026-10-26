// Package troopersarama instruments github.com/IBM/sarama for TaskTrooper.
//
// Consumer: either wrap each message with ProcessMessage, or let NewConsumerGroupHandler drive
// the claim loop (one consumer entry span per message, message marked after processing):
//
//	h := troopersarama.NewConsumerGroupHandler(func(ctx context.Context, m *sarama.ConsumerMessage) error {
//		return handle(ctx, m)
//	}, troopersarama.WithConsumerGroup("orders"), troopersarama.WithProcess("orders-worker"))
//	for { _ = group.Consume(ctx, []string{"orders.created"}, h) }
//
// Producer: SendMessage wraps a SyncProducer send with a producer span and injects the trace
// context + baggage (tt.svc) into the message headers. For async producers, register
// ProducerInterceptor (config.Producer.Interceptors) so that at least the caller baggage is
// propagated, or call InjectContext(ctx, msg) before sending.
//
// Message keys and values are never recorded.
package troopersarama
