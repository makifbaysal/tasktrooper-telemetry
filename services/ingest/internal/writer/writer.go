package writer

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tasktrooper/telemetry/services/ingest/internal/geo"
	"github.com/tasktrooper/telemetry/services/ingest/internal/obs"
)

// Config holds ingest-writer settings.
type Config struct {
	Brokers       []string
	Group         string
	Topics        []string
	BatchRows     int           // flush when any table batch reaches this many rows
	FlushInterval time.Duration // ...or when the oldest buffered record is this old
	MaxPoll       int           // records per poll
	RetryMax      time.Duration // cap of the insert retry backoff
	Sampler       TailSampler   // tail sampling of spans (Rate ≥ 1 or 0 = keep everything)
	Geo           *geo.DB       // optional GeoIP database for RUM country
	Blobs         BlobStore     // session replay segments (S3); nil → tt.replay must not be consumed
	BlobWorkers   int           // parallel uploads per flush (default 16)
}

// BlobStore stores replay segment objects. Put must be idempotent (same key, same body).
type BlobStore interface {
	Put(ctx context.Context, key string, body []byte, contentType, contentEncoding string) error
}

// Writer consumes, flattens, inserts and commits.
type Writer struct {
	cfg Config
	ins Inserter
	log *slog.Logger
	cl  *kgo.Client

	mu      sync.Mutex // guards everything below (main loop vs. revoke callback)
	b       *Batches
	pending map[tp]*kgo.Record // highest record per partition not yet committed
	first   time.Time          // when the oldest buffered record arrived
}

type tp struct {
	topic     string
	partition int32
}

// New creates a Writer and its consumer-group client (manual commits, rebalances blocked while processing).
func New(cfg Config, ins Inserter, log *slog.Logger) (*Writer, error) {
	if cfg.BatchRows <= 0 {
		cfg.BatchRows = 50_000
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = time.Second
	}
	if cfg.MaxPoll <= 0 {
		cfg.MaxPoll = 1000
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = 30 * time.Second
	}
	if cfg.BlobWorkers <= 0 {
		cfg.BlobWorkers = 16
	}
	w := &Writer{cfg: cfg, ins: ins, log: log, b: NewBatches(), pending: map[tp]*kgo.Record{}}
	w.b.Geo = cfg.Geo
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ConsumerGroup(cfg.Group),
		kgo.ConsumeTopics(cfg.Topics...),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.FetchMaxBytes(64<<20),
		kgo.OnPartitionsRevoked(w.onRevoked),
		kgo.OnPartitionsLost(w.onLost),
	)
	if err != nil {
		return nil, err
	}
	w.cl = cl
	return w, nil
}

// Run consumes until ctx is cancelled, then flushes what it has and commits.
func (w *Writer) Run(ctx context.Context) error {
	defer w.cl.Close()
	for {
		if ctx.Err() != nil {
			w.shutdown()
			return nil
		}
		pollCtx, cancel := context.WithTimeout(ctx, w.pollWait())
		fetches := w.cl.PollRecords(pollCtx, w.cfg.MaxPoll)
		cancel()
		if fetches.IsClientClosed() {
			return nil
		}
		fetches.EachError(func(topic string, p int32, err error) {
			if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				w.log.Warn("fetch error", "topic", topic, "partition", p, "err", err)
			}
		})
		fetches.EachPartition(func(p kgo.FetchTopicPartition) {
			if n := len(p.Records); n > 0 {
				lag := p.HighWatermark - (p.Records[n-1].Offset + 1)
				obs.WriterConsumerLag.WithLabelValues(p.Topic, strconv.Itoa(int(p.Partition))).Set(float64(max(lag, 0)))
			}
		})
		w.mu.Lock()
		fetches.EachRecord(func(r *kgo.Record) {
			obs.WriterConsumed.WithLabelValues(r.Topic).Inc()
			if err := w.b.Add(r); err != nil {
				obs.WriterDecodeErrors.WithLabelValues(r.Topic).Inc()
				w.log.Warn("skipping undecodable record", "topic", r.Topic, "partition", r.Partition, "offset", r.Offset, "err", err)
			}
			if w.first.IsZero() {
				w.first = time.Now()
			}
			w.pending[tp{r.Topic, r.Partition}] = r
		})
		due := len(w.pending) > 0 && (w.b.Rows() >= w.cfg.BatchRows || w.b.Full() || time.Since(w.first) >= w.cfg.FlushInterval)
		var err error
		if due {
			err = w.flushLocked(ctx)
		}
		w.mu.Unlock()
		w.cl.AllowRebalance()
		if err != nil && ctx.Err() == nil {
			return err
		}
	}
}

// pollWait bounds a poll so the flush interval is honored even when traffic stops.
func (w *Writer) pollWait() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.first.IsZero() {
		return w.cfg.FlushInterval
	}
	return max(time.Millisecond, w.cfg.FlushInterval-time.Since(w.first))
}

// flushLocked inserts every table batch (retrying each until it succeeds or ctx ends) and then commits.
func (w *Writer) flushLocked(ctx context.Context) error {
	if len(w.pending) == 0 {
		return nil
	}
	start := time.Now()
	w.b.Sample(w.cfg.Sampler)
	// Replay blobs first: an index row must never point at a missing object. Failed uploads are retried like inserts
	// and nothing is committed until every blob and row is stored (at-least-once; a redelivery rewrites the same key).
	if err := w.uploadBlobs(ctx); err != nil {
		return err
	}
	tables := w.b.tables()
	var wg sync.WaitGroup
	errs := make([]error, len(tables))
	for i, t := range tables {
		wg.Go(func() { errs[i] = w.insertWithRetry(ctx, t) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err // not committed: records are redelivered after restart (at-least-once)
	}
	recs := make([]*kgo.Record, 0, len(w.pending))
	for _, r := range w.pending {
		recs = append(recs, r)
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := w.cl.CommitRecords(cctx, recs...); err != nil {
		// Rows are in ClickHouse; a failed commit only means duplicates after a rebalance/restart.
		w.log.Warn("offset commit failed", "err", err)
	}
	obs.WriterFlushDuration.Observe(time.Since(start).Seconds())
	attrs := []any{"ms", time.Since(start).Milliseconds()}
	for _, t := range tables {
		attrs = append(attrs, t.table, t.n)
	}
	w.log.Debug("flushed", attrs...)
	w.b.Reset()
	clear(w.pending)
	w.first = time.Time{}
	return nil
}

func (w *Writer) uploadBlobs(ctx context.Context) error {
	blobs := w.b.Blobs
	if len(blobs) == 0 {
		return nil
	}
	if w.cfg.Blobs == nil {
		w.log.Warn("replay segments without a blob store dropped", "segments", len(blobs))
		obs.WriterDecodeErrors.WithLabelValues("tt.replay").Add(float64(len(blobs)))
		clear(w.b.Replay)
		w.b.Replay = w.b.Replay[:0]
		clear(w.b.Blobs)
		w.b.Blobs = w.b.Blobs[:0]
		w.b.blobBytes = 0
		return nil
	}
	start := time.Now()
	sem := make(chan struct{}, w.cfg.BlobWorkers)
	var wg sync.WaitGroup
	errs := make([]error, len(blobs))
	for i := range blobs {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			errs[i] = w.putWithRetry(ctx, blobs[i])
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	obs.WriterBlobDuration.Observe(time.Since(start).Seconds())
	return nil
}

func (w *Writer) putWithRetry(ctx context.Context, b Blob) error {
	backoff := 200 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := w.cfg.Blobs.Put(ctx, b.Key, b.Body, "application/json", "gzip")
		if err == nil {
			obs.WriterBlobsStored.Inc()
			obs.WriterBlobBytes.Add(float64(len(b.Body)))
			return nil
		}
		obs.WriterBlobErrors.Inc()
		w.log.Error("replay blob upload failed", "key", b.Key, "attempt", attempt, "retry_in", backoff.String(), "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, w.cfg.RetryMax)
	}
}

func (w *Writer) insertWithRetry(ctx context.Context, t tableBatch) error {
	backoff := 200 * time.Millisecond
	for attempt := 1; ; attempt++ {
		start := time.Now()
		err := w.ins.Insert(ctx, t.table, t.columns, t.n, t.row)
		if err == nil {
			obs.WriterInsertDuration.WithLabelValues(t.table).Observe(time.Since(start).Seconds())
			obs.WriterRowsInserted.WithLabelValues(t.table).Add(float64(t.n))
			return nil
		}
		obs.WriterInsertErrors.WithLabelValues(t.table).Inc()
		w.log.Error("clickhouse insert failed", "table", t.table, "rows", t.n, "attempt", attempt, "retry_in", backoff.String(), "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, w.cfg.RetryMax)
	}
}

// onRevoked flushes and commits before partitions move to another consumer.
func (w *Writer) onRevoked(ctx context.Context, _ *kgo.Client, _ map[string][]int32) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.flushLocked(ctx); err != nil {
		w.log.Warn("flush on revoke failed", "err", err)
	}
}

// onLost forgets offsets of partitions we no longer own (their rows may be inserted twice; at-least-once).
func (w *Writer) onLost(_ context.Context, _ *kgo.Client, lost map[string][]int32) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for topic, ps := range lost {
		for _, p := range ps {
			delete(w.pending, tp{topic, p})
		}
	}
}

// shutdown flushes buffered rows with a bounded deadline.
func (w *Writer) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.flushLocked(ctx); err != nil {
		w.log.Warn("final flush failed; records will be redelivered", "err", err)
	}
}
