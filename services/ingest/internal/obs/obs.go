// Package obs holds the Prometheus metrics of ingest-gw and ingest-writer (served on /metrics).
// Labels are low-cardinality only: endpoint path, status code, topic, table, reason.
package obs

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var latencyBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}

// ── ingest-gw ──
var (
	GWRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tt_ingest_gw_requests_total", Help: "HTTP/gRPC ingest requests by endpoint and status code.",
	}, []string{"endpoint", "status"})
	GWRequestBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tt_ingest_gw_request_bytes_total", Help: "Request body bytes as received on the wire.",
	}, []string{"endpoint"})
	GWRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "tt_ingest_gw_request_duration_seconds", Help: "Ingest request handling time.", Buckets: latencyBuckets,
	}, []string{"endpoint"})
	GWAuthFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tt_ingest_gw_auth_failures_total", Help: "Rejected requests: invalid key, origin, integration or drain signature.",
	}, []string{"endpoint", "reason"})
	GWRateLimited = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tt_ingest_gw_rate_limited_total", Help: "Requests answered with 429.",
	}, []string{"endpoint"})
	GWProduced = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tt_ingest_gw_produced_records_total", Help: "Records acknowledged by Redpanda.",
	}, []string{"topic"})
	GWProduceErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tt_ingest_gw_produce_errors_total", Help: "Records that failed (reason=failed) or were rejected because the buffer was full (reason=buffer_full).",
	}, []string{"topic", "reason"})
	GWProduceLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "tt_ingest_gw_produce_latency_seconds", Help: "Enqueue → broker ack latency.", Buckets: latencyBuckets,
	}, []string{"topic"})
	GWDrainEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tt_ingest_gw_drain_events_total", Help: "Vercel drain entries accepted by kind (log, trace, speed_insights, web_analytics, unknown).",
	}, []string{"provider", "kind"})
)

// ── ingest-writer ──
var (
	WriterConsumed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tt_ingest_writer_records_consumed_total", Help: "Kafka records consumed.",
	}, []string{"topic"})
	WriterDecodeErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tt_ingest_writer_decode_errors_total", Help: "Records skipped because they could not be decoded.",
	}, []string{"topic"})
	WriterRowsInserted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tt_ingest_writer_rows_inserted_total", Help: "Rows inserted into ClickHouse per table.",
	}, []string{"table"})
	WriterInsertErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tt_ingest_writer_clickhouse_errors_total", Help: "Failed ClickHouse insert attempts per table (each is retried).",
	}, []string{"table"})
	WriterInsertDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "tt_ingest_writer_insert_duration_seconds", Help: "Successful ClickHouse insert time per table.", Buckets: latencyBuckets,
	}, []string{"table"})
	WriterFlushDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "tt_ingest_writer_batch_duration_seconds", Help: "Whole flush (all tables + offset commit).", Buckets: latencyBuckets,
	})
	WriterConsumerLag = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tt_ingest_writer_consumer_lag", Help: "High watermark minus next offset to consume, per topic partition.",
	}, []string{"topic", "partition"})
	WriterBlobsStored = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tt_ingest_writer_replay_blobs_total", Help: "Session replay segments stored in S3.",
	})
	WriterBlobBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tt_ingest_writer_replay_blob_bytes_total", Help: "Compressed bytes of session replay segments stored in S3.",
	})
	WriterBlobErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tt_ingest_writer_replay_blob_errors_total", Help: "Failed S3 uploads of replay segments (each is retried).",
	})
	WriterBlobDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "tt_ingest_writer_replay_blob_upload_seconds", Help: "Upload time of all replay segments of one flush.", Buckets: latencyBuckets,
	})
	WriterReplayDisabled = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "tt_writer_replay_disabled", Help: "1 when S3 is not configured / not reachable at start and tt.replay is not consumed (contracts §23).",
	})
	WriterSpansSampled = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tt_ingest_writer_tail_sampling_spans_total", Help: "Tail sampling decisions per span (keep_forced = error/slow trace, keep_sampled, drop).",
	}, []string{"decision"})
)

// Handler serves the default registry (also Go runtime and process metrics).
func Handler() http.Handler { return promhttp.Handler() }

// Serve starts a /metrics listener on addr in the background. Failures are logged, never fatal.
// The returned func shuts it down.
func Serve(addr string, log *slog.Logger) func() {
	if addr == "" {
		return func() {}
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", Handler())
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Warn("metrics listener disabled", "addr", addr, "err", err)
		return func() {}
	}
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("metrics server", "err", err)
		}
	}()
	log.Info("metrics listening", "addr", ln.Addr().String())
	return func() { _ = hs.Close() }
}
