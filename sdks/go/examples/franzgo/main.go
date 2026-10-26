// Command franzgo is an example franz-go consumer instrumented with trooper-go.
//
//	TROOPER_DSN=http://tt_dev_ingest_key@localhost:4318/1 KAFKA_BROKERS=localhost:19092 go run ./franzgo
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	trooper "github.com/tasktrooper/telemetry-go"
	trooperfranz "github.com/tasktrooper/telemetry-go/integrations/franzgo"
)

const (
	topic = "orders.created"
	group = "billing-worker"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	shutdown, err := trooper.Start(ctx, trooper.Config{Service: "billing-worker", Environment: "dev"})
	if err != nil {
		log.Printf("telemetry disabled: %v", err)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(c)
	}()

	tt := trooperfranz.New(
		trooperfranz.WithConsumerGroup(group),
		trooperfranz.WithProcess("charge-order"), // trooper.process / trooper.entry.route
		trooperfranz.WithAttemptHeader("x-attempt"),
	)
	brokers := strings.Split(envOr("KAFKA_BROKERS", "localhost:19092"), ",")
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topic),
		kgo.WithHooks(tt.Hooks()...), // producer spans + header injection
	)
	if err != nil {
		log.Fatal(err)
	}
	defer cl.Close()

	for {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			return
		}
		fetches.EachError(func(t string, p int32, err error) { log.Printf("fetch %s/%d: %v", t, p, err) })
		fetches.EachRecord(func(rec *kgo.Record) {
			err := tt.ProcessRecord(ctx, rec, func(ctx context.Context) error {
				return handle(ctx, cl, rec)
			})
			if err != nil {
				log.Printf("process offset %d: %v", rec.Offset, err)
			}
		})
	}
}

func handle(ctx context.Context, cl *kgo.Client, rec *kgo.Record) error {
	if len(rec.Value) == 0 {
		// Give up on poison messages: forward to the DLQ and flag the consumer span.
		trooper.MarkDLQ(ctx)
		cl.Produce(ctx, &kgo.Record{Topic: topic + ".dlq", Value: rec.Value}, nil)
		return errors.New("empty payload")
	}
	// Downstream event: producer span is a child of the consumer span and carries trooper.entry.route.
	cl.Produce(ctx, &kgo.Record{Topic: "invoices.created", Value: rec.Value}, nil)
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
