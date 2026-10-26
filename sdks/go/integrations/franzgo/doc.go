// Package trooperfranz instruments github.com/twmb/franz-go (kgo) for TaskTrooper.
//
// Producer: t.Hooks() adds a producer span per record and injects W3C trace context and baggage
// (tt.svc=<service>) into the record headers.
//
// Consumer: wrap the processing of each record with t.ProcessRecord. It extracts the trace context
// from the headers and runs fn inside a consumer entry span carrying trooper.process,
// trooper.consumer.attempt, messaging.* attributes and trooper.caller.
//
//	t := trooperfranz.New(trooperfranz.WithConsumerGroup("orders"), trooperfranz.WithProcess("orders-worker"))
//	cl, _ := kgo.NewClient(kgo.SeedBrokers(b), kgo.ConsumerGroup("orders"), kgo.ConsumeTopics("orders.created"),
//		kgo.WithHooks(t.Hooks()...))
//	fetches := cl.PollFetches(ctx)
//	fetches.EachRecord(func(r *kgo.Record) {
//		_ = t.ProcessRecord(ctx, r, func(ctx context.Context) error { return handle(ctx, r) })
//	})
//
// Record keys and values are never recorded.
package trooperfranz
