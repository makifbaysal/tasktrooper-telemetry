package gw

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tasktrooper/telemetry/services/ingest/internal/obs"
)

// ErrBufferFull means the producer buffer is saturated (Redpanda slow/down); callers answer 503.
var ErrBufferFull = errors.New("producer buffer full")

// Sink accepts records for asynchronous delivery. Send must not block on a full buffer.
type Sink interface {
	Send(r *kgo.Record) error
}

// KafkaSink produces to Redpanda with franz-go, asynchronously, with a bounded buffer.
type KafkaSink struct {
	cl                *kgo.Client
	log               *slog.Logger
	maxRecs, maxBytes int64

	Produced, Failed, Rejected atomic.Int64
}

// ProducerConfig tunes the Kafka producer.
type ProducerConfig struct {
	Brokers         []string
	BufferRecords   int           // max records waiting for ack before Send returns ErrBufferFull
	BufferBytes     int           // max bytes waiting
	DeliveryTimeout time.Duration // give up on a record after this long (drains the buffer during outages)
}

// NewKafkaSink connects a producer.
func NewKafkaSink(cfg ProducerConfig, log *slog.Logger) (*KafkaSink, error) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.MaxBufferedRecords(cfg.BufferRecords+1024), // headroom over the Send check
		kgo.MaxBufferedBytes(cfg.BufferBytes+16<<20),
		kgo.ProducerBatchCompression(kgo.ZstdCompression(), kgo.Lz4Compression(), kgo.NoCompression()),
		kgo.ProducerLinger(5*time.Millisecond),
		kgo.RecordDeliveryTimeout(cfg.DeliveryTimeout),
		kgo.ProducerBatchMaxBytes(1<<20),
	)
	if err != nil {
		return nil, err
	}
	return &KafkaSink{cl: cl, log: log, maxRecs: int64(cfg.BufferRecords), maxBytes: int64(cfg.BufferBytes)}, nil
}

// Ping checks broker connectivity.
func (k *KafkaSink) Ping(ctx context.Context) error { return k.cl.Ping(ctx) }

// Send implements Sink. It rejects up front when the buffer is (nearly) full so requests never block;
// the rare race past that check fails in the promise with ErrMaxBuffered (counted as rejected).
func (k *KafkaSink) Send(r *kgo.Record) error {
	if k.cl.BufferedProduceRecords() >= k.maxRecs || k.cl.BufferedProduceBytes()+int64(len(r.Value)) >= k.maxBytes {
		k.Rejected.Add(1)
		return ErrBufferFull
	}
	k.cl.TryProduce(context.Background(), r, k.done)
	return nil
}

func (k *KafkaSink) done(r *kgo.Record, err error) {
	if errors.Is(err, kgo.ErrMaxBuffered) {
		k.Rejected.Add(1)
		obs.GWProduceErrors.WithLabelValues(r.Topic, "buffer_full").Inc()
		return
	}
	if err != nil {
		k.Failed.Add(1)
		obs.GWProduceErrors.WithLabelValues(r.Topic, "failed").Inc()
		k.log.Error("produce failed", "topic", r.Topic, "bytes", len(r.Value), "err", err)
		return
	}
	k.Produced.Add(1)
	obs.GWProduced.WithLabelValues(r.Topic).Inc()
	if !r.Timestamp.IsZero() {
		obs.GWProduceLatency.WithLabelValues(r.Topic).Observe(time.Since(r.Timestamp).Seconds())
	}
}

// Close flushes buffered records (bounded by ctx) and closes the client.
func (k *KafkaSink) Close(ctx context.Context) {
	if err := k.cl.Flush(ctx); err != nil {
		k.log.Warn("producer flush incomplete", "err", err)
	}
	k.cl.Close()
}

func headerValue(v int64) []byte { return strconv.AppendInt(nil, v, 10) }
