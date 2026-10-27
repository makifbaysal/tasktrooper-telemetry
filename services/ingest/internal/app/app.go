// Package app wires ingest-gw and ingest-writer from configuration; cmd/* and the e2e test use it.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/tasktrooper/telemetry/services/ingest/internal/auth"
	"github.com/tasktrooper/telemetry/services/ingest/internal/geo"
	"github.com/tasktrooper/telemetry/services/ingest/internal/gw"
	"github.com/tasktrooper/telemetry/services/ingest/internal/obs"
	"github.com/tasktrooper/telemetry/services/ingest/internal/ratelimit"
	"github.com/tasktrooper/telemetry/services/ingest/internal/writer"
	"github.com/tasktrooper/telemetry/services/pkg/config"
	"github.com/tasktrooper/telemetry/services/pkg/secrets"
	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

// GatewayConfig is the full ingest-gw configuration.
type GatewayConfig struct {
	Common   config.Common
	GW       gw.Config
	Producer gw.ProducerConfig
	Burst    float64 // rate-limit burst = rps × Burst
	// MetricsAddr serves /metrics on its own listener; empty → /metrics on the HTTP port.
	MetricsAddr string
	// InternalToken (TT_INTERNAL_TOKEN) enables the internal service credential (contracts §16); empty = off.
	InternalToken string
}

// GatewayFromEnv reads TT_* variables.
func GatewayFromEnv() GatewayConfig {
	c := config.Load()
	return GatewayConfig{
		Common: c,
		GW: gw.Config{
			HTTPAddr:       config.String("TT_INGEST_HTTP_ADDR", ":4318"),
			GRPCAddr:       config.String("TT_INGEST_GRPC_ADDR", ":4317"),
			MaxBodyBytes:   int64(config.Int("TT_INGEST_MAX_BODY_BYTES", 5<<20)),
			MaxRecordBytes: config.Int("TT_INGEST_MAX_RECORD_BYTES", 900<<10),
			DefaultRPS:     config.Int("TT_INGEST_RATE_LIMIT_RPS", 1000),
		},
		Producer: gw.ProducerConfig{
			Brokers:         c.KafkaBrokers,
			BufferRecords:   config.Int("TT_INGEST_PRODUCE_BUFFER_RECORDS", 20000),
			BufferBytes:     config.Int("TT_INGEST_PRODUCE_BUFFER_BYTES", 256<<20),
			DeliveryTimeout: config.Duration("TT_INGEST_PRODUCE_TIMEOUT", 30*time.Second),
		},
		Burst:         2,
		MetricsAddr:   config.String("TT_INGEST_METRICS_ADDR", ""),
		InternalToken: config.String("TT_INTERNAL_TOKEN", ""),
	}
}

// RunGateway serves until ctx is cancelled. ready (optional) receives the bound HTTP and gRPC addresses.
func RunGateway(ctx context.Context, cfg GatewayConfig, log *slog.Logger, ready func(httpAddr, grpcAddr string)) error {
	pool, err := pgxpool.New(ctx, cfg.Common.PostgresURL)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()
	keys := auth.NewCache(auth.PG{Pool: pool}, log)
	go keys.Run(ctx)

	var rdb *redis.Client
	if opt, err := redis.ParseURL(cfg.Common.RedisURL); err != nil {
		log.Warn("invalid TT_REDIS_URL, rate limiting in-memory only", "err", err)
	} else {
		opt.DialTimeout, opt.ReadTimeout, opt.WriteTimeout = 200*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond
		rdb = redis.NewClient(opt)
		defer rdb.Close()
	}
	var lim *ratelimit.Limiter
	if rdb != nil {
		lim = ratelimit.New(rdb, cfg.Burst, log)
	} else {
		lim = ratelimit.New(nil, cfg.Burst, log)
	}

	sink, err := gw.NewKafkaSink(cfg.Producer, log)
	if err != nil {
		return fmt.Errorf("kafka: %w", err)
	}
	if cfg.MetricsAddr != "" {
		cfg.GW.ServeMetrics = false
		defer obs.Serve(cfg.MetricsAddr, log)()
	} else {
		cfg.GW.ServeMetrics = true
	}
	srv := gw.New(cfg.GW, keys, lim, sink, log)
	if in := auth.NewInternal(cfg.InternalToken, auth.PGProjects{Pool: pool}, log); in != nil {
		srv.SetInternal(in)
		log.Info("internal service credential enabled")
	}
	if skey, err := secrets.Key(); err != nil {
		log.Warn("TT_SECRETS_KEY invalid; drains disabled", "err", err)
	} else {
		srv.SetIntegrations(gw.NewIntegrationCache(gw.PGIntegrations{Pool: pool, Key: skey, Log: log}, log))
	}

	hl, err := net.Listen("tcp", cfg.GW.HTTPAddr)
	if err != nil {
		return err
	}
	gl, err := net.Listen("tcp", cfg.GW.GRPCAddr)
	if err != nil {
		_ = hl.Close()
		return err
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	gs := srv.GRPCServer()
	errc := make(chan error, 2)
	go func() { errc <- hs.Serve(hl) }()
	go func() { errc <- gs.Serve(gl) }()
	log.Info("ingest-gw listening", "http", hl.Addr().String(), "grpc", gl.Addr().String())
	if ready != nil {
		ready(hl.Addr().String(), gl.Addr().String())
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = hs.Shutdown(sctx)
	stopped := make(chan struct{})
	go func() { gs.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-sctx.Done():
		gs.Stop()
	}
	sink.Close(sctx)
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return serveErr
}

// WriterConfig is the full ingest-writer configuration.
type WriterConfig struct {
	Common      config.Common
	Writer      writer.Config
	HealthAddr  string
	MetricsAddr string // Prometheus /metrics; empty disables
	GeoIPDB     string // mmdb path (MaxMind GeoLite2-Country / DB-IP); empty → no country
}

// WriterFromEnv reads TT_* variables.
func WriterFromEnv() WriterConfig {
	c := config.Load()
	return WriterConfig{
		Common: c,
		Writer: writer.Config{
			Brokers:       c.KafkaBrokers,
			Group:         config.String("TT_INGEST_WRITER_GROUP", topics.WriterGroup),
			Topics:        config.List("TT_INGEST_WRITER_TOPICS", "tt.otlp.traces,tt.otlp.metrics,tt.otlp.logs,tt.rum"),
			BatchRows:     config.Int("TT_INGEST_WRITER_BATCH_ROWS", 50_000),
			FlushInterval: config.Duration("TT_INGEST_WRITER_FLUSH_INTERVAL", time.Second),
			MaxPoll:       config.Int("TT_INGEST_WRITER_MAX_POLL", 5000),
			RetryMax:      config.Duration("TT_INGEST_WRITER_RETRY_MAX", 30*time.Second),
			Sampler: writer.TailSampler{
				Rate:   envFloat("TT_TAIL_SAMPLE_RATE", 1),
				SlowNs: uint64(max(config.Int("TT_TAIL_SLOW_MS", 1000), 0)) * uint64(time.Millisecond),
			},
		},
		HealthAddr:  config.String("TT_INGEST_WRITER_HEALTH_ADDR", ":4320"),
		MetricsAddr: config.String("TT_INGEST_WRITER_METRICS_ADDR", ":9464"),
		GeoIPDB:     config.String("TT_GEOIP_DB", ""),
	}
}

// RunWriter consumes until ctx is cancelled.
func RunWriter(ctx context.Context, cfg WriterConfig, log *slog.Logger) error {
	ch, err := openClickHouseRetry(ctx, cfg.Common.ClickHouseURL, log)
	if err != nil {
		return err
	}
	defer ch.Close()
	if cfg.GeoIPDB != "" {
		db, err := geo.Open(cfg.GeoIPDB)
		if err != nil {
			log.Warn("GeoIP database not loaded; country stays empty", "path", cfg.GeoIPDB, "err", err)
		} else {
			defer db.Close()
			cfg.Writer.Geo = db
		}
	}
	if r := cfg.Writer.Sampler.Rate; r <= 0 || r > 1 {
		cfg.Writer.Sampler.Rate = 1
	}
	defer obs.Serve(cfg.MetricsAddr, log)()
	w, err := writer.New(cfg.Writer, ch, log)
	if err != nil {
		return fmt.Errorf("kafka: %w", err)
	}
	if cfg.HealthAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(rw http.ResponseWriter, r *http.Request) {
			if err := ch.Conn.Ping(r.Context()); err != nil {
				http.Error(rw, "clickhouse: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
			_, _ = rw.Write([]byte("ok"))
		})
		hs := &http.Server{Addr: cfg.HealthAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Warn("health server", "err", err)
			}
		}()
		defer hs.Close()
	}
	log.Info("ingest-writer consuming", "group", cfg.Writer.Group, "topics", cfg.Writer.Topics,
		"tail_sample_rate", cfg.Writer.Sampler.Rate, "geoip", cfg.Writer.Geo != nil)
	return w.Run(ctx)
}

func openClickHouseRetry(ctx context.Context, dsn string, log *slog.Logger) (*writer.ClickHouse, error) {
	backoff := time.Second
	for {
		ch, err := writer.OpenClickHouse(ctx, dsn)
		if err == nil {
			return ch, nil
		}
		log.Warn("clickhouse not reachable, retrying", "err", err, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// envFloat reads a float env var (services/pkg/config has no Float helper).
func envFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(key)), 64); err == nil {
		return v
	}
	return def
}
