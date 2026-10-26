// Command trooper-cloud runs cloud integrations (AWS, Google Cloud, Neon, Vercel — ADR-012): discovery into
// Postgres databases / broker_clusters / deploy_markers and metrics to ingest-gw as OTLP (contracts §5, §10).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/config"
	"github.com/tasktrooper/telemetry/services/pkg/logx"
	"github.com/tasktrooper/telemetry/services/pkg/secrets"

	"github.com/tasktrooper/telemetry/services/integrations/internal/factory"
	"github.com/tasktrooper/telemetry/services/integrations/internal/otlp"
	"github.com/tasktrooper/telemetry/services/integrations/internal/runner"
	"github.com/tasktrooper/telemetry/services/integrations/internal/store"
)

var version = "dev"

func main() {
	once := flag.Int64("once", 0, "run discovery + one metrics step for this integration id, then exit")
	flag.Parse()
	if err := run(*once); err != nil {
		fmt.Fprintln(os.Stderr, "trooper-cloud:", err)
		os.Exit(1)
	}
}

func run(once int64) error {
	common := config.Load()
	log := logx.New("trooper-cloud", common.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	metricsURL, dsnKey, dsnProject, err := otlp.ParseDSN(config.String("TT_DSN", "http://tt_dev_ingest_key@localhost:4318/1"))
	if err != nil {
		return err
	}
	keys, err := otlp.ParseKeys(os.Getenv("TT_CLOUD_INGEST_KEYS"))
	if err != nil {
		return err
	}
	if _, ok := keys[dsnProject]; !ok && dsnKey != "" && dsnProject > 0 {
		keys[dsnProject] = dsnKey
	}
	skey, err := secrets.Key()
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, common.PostgresURL)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer st.Close()

	f := &factory.Factory{
		Key: skey, Store: st, IngestKeys: keys, PublicIngestURL: os.Getenv("TT_PUBLIC_INGEST_URL"),
		Endpoints: factory.Endpoints{
			AWS: os.Getenv("TT_CLOUD_AWS_ENDPOINT"), GCP: os.Getenv("TT_CLOUD_GCP_ENDPOINT"),
			Neon: os.Getenv("TT_CLOUD_NEON_URL"), Vercel: os.Getenv("TT_CLOUD_VERCEL_URL"),
		},
	}
	r := runner.New(runner.Options{
		Store:    st,
		Sender:   &otlp.Sender{MetricsURL: metricsURL, Keys: keys, Version: version},
		Factory:  f.Build,
		Log:      log,
		Reload:   config.Duration("TT_CLOUD_RELOAD_INTERVAL", 30*time.Second),
		Discover: config.Duration("TT_CLOUD_DISCOVERY_INTERVAL", 10*time.Minute),
		Metrics:  config.Duration("TT_CLOUD_METRICS_INTERVAL", 60*time.Second),
	})
	log.Info("trooper-cloud starting", "version", version, "metrics_url", metricsURL, "projects_with_keys", len(keys))
	if once > 0 {
		list, err := st.Integrations(ctx)
		if err != nil {
			return err
		}
		for _, in := range list {
			if in.ID == once {
				return r.RunOnce(ctx, in)
			}
		}
		return fmt.Errorf("integration %d not found or disabled", once)
	}
	return r.Run(ctx)
}
