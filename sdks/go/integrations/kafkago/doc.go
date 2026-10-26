// Package trooperkafkago instruments github.com/segmentio/kafka-go for TaskTrooper.
//
//	// consumer
//	m, _ := reader.FetchMessage(ctx)
//	err := trooperkafkago.ProcessMessage(ctx, m, func(ctx context.Context) error { return handle(ctx, m) },
//		trooperkafkago.WithConsumerGroup("orders"), trooperkafkago.WithProcess("orders-worker"))
//
//	// producer: one producer span per message, trace context + baggage injected into headers
//	err := trooperkafkago.WriteMessages(ctx, writer, kafka.Message{Value: b})
//
// Message keys and values are never recorded.
package trooperkafkago
