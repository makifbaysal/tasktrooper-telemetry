// Command trooper-cloud runs cloud integrations (AWS, Google Cloud, Neon, Vercel — ADR-012): discovery into
// Postgres databases / broker_clusters / deploy_markers and metrics to ingest-gw as OTLP (contracts §5, §10).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/config"
	"github.com/tasktrooper/telemetry/services/pkg/logx"
	"github.com/tasktrooper/telemetry/services/pkg/secrets"

	"github.com/tasktrooper/telemetry/services/integrations/internal/factory"
	"github.com/tasktrooper/telemetry/services/integrations/internal/httpx"
	"github.com/tasktrooper/telemetry/services/integrations/internal/otlp"
	"github.com/tasktrooper/telemetry/services/integrations/internal/runner"
	"github.com/tasktrooper/telemetry/services/integrations/internal/selftel"
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
	// Self telemetry (ADR-027): a no-op unless TROOPER_DSN is set (not TT_DSN, which is where cloud metrics go).
	flush := selftel.Start(context.Background(), version, log)
	defer flush()
	log = selftel.Logger(log)
	httpx.Transport = selftel.Transport() // provider API calls only; otlp.Sender keeps its own untraced client
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Keyless default: metrics go with TT_INTERNAL_TOKEN (or per-project TT_CLOUD_INGEST_KEYS); no seeded key exists (ADR-028).
	metricsURL, dsnKey, dsnProject, err := otlp.ParseDSN(config.String("TT_DSN", "http://localhost:4318"))
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
	// TT_INTERNAL_TOKEN: internal service credential shared with ingest-gw (contracts §16); keys stay as fallback.
	internalToken := strings.TrimSpace(os.Getenv("TT_INTERNAL_TOKEN"))
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
		Log: log, HTTP: selftel.Client(30 * time.Second),
		Endpoints: factory.Endpoints{
			AWS: os.Getenv("TT_CLOUD_AWS_ENDPOINT"), GCP: os.Getenv("TT_CLOUD_GCP_ENDPOINT"),
			Neon: os.Getenv("TT_CLOUD_NEON_URL"), Vercel: os.Getenv("TT_CLOUD_VERCEL_URL"),
			AzureLogin: os.Getenv("TT_AZURE_LOGIN_URL"), AzureARM: os.Getenv("TT_AZURE_ARM_URL"),
		},
	}
	r := runner.New(runner.Options{
		Store:    st,
		Sender:   &otlp.Sender{MetricsURL: metricsURL, InternalToken: internalToken, Keys: keys, Version: version},
		Factory:  f.Build,
		Log:      log,
		Reload:   config.Duration("TT_CLOUD_RELOAD_INTERVAL", 30*time.Second),
		Discover: config.Duration("TT_CLOUD_DISCOVERY_INTERVAL", 10*time.Minute),
		Metrics:  config.Duration("TT_CLOUD_METRICS_INTERVAL", 60*time.Second),
	})
	log.Info("trooper-cloud starting", "version", version, "metrics_url", metricsURL, "projects_with_keys", len(keys), "internal_credential", internalToken != "")
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
