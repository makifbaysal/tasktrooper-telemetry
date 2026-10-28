package main

import (
	"context"

	"github.com/IBM/sarama"
	"github.com/twmb/franz-go/pkg/kgo"
)

func consume(ctx context.Context) {
	group, _ := sarama.NewConsumerGroup([]string{"b:9092"}, "order-events.fulfilment", nil)
	_ = group.Consume(ctx, []string{"orders.created"}, handler{})
	_, _ = kgo.NewClient(kgo.ConsumerGroup("payments-dlq-replayer"), kgo.ConsumeTopics("payments.dlq"))
}

type handler struct{}
