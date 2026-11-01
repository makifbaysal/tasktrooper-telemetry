// Command ingest-gw accepts OTLP (HTTP :4318, gRPC :4317) and RUM beacons and publishes them to Redpanda.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tasktrooper/telemetry/services/ingest/internal/app"
	"github.com/tasktrooper/telemetry/services/ingest/internal/selfobs"
	"github.com/tasktrooper/telemetry/services/pkg/logx"
)

const service = "tt-ingest-gw"

func main() {
	cfg := app.GatewayFromEnv()
	log := logx.New("ingest-gw", cfg.Common.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Self-telemetry (ADR-027): only with TROOPER_DSN; otherwise nothing is installed.
	shutdown, enabled, err := selfobs.Start(ctx, service)
	if err != nil {
		log.Warn("self-telemetry disabled", "err", err)
	}
	if enabled {
		log = slog.New(selfobs.LogHandler(log.Handler(), service))
		cfg.Telemetry = true
	}
	err = app.RunGateway(ctx, cfg, log, nil)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = shutdown(sctx)
	cancel()
	if err != nil {
		log.Error("ingest-gw stopped", "err", err)
		os.Exit(1)
	}
}
