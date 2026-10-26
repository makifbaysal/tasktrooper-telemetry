// Command ingest-gw accepts OTLP (HTTP :4318, gRPC :4317) and RUM beacons and publishes them to Redpanda.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/tasktrooper/telemetry/services/ingest/internal/app"
	"github.com/tasktrooper/telemetry/services/pkg/logx"
)

func main() {
	cfg := app.GatewayFromEnv()
	log := logx.New("ingest-gw", cfg.Common.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.RunGateway(ctx, cfg, log, nil); err != nil {
		log.Error("ingest-gw stopped", "err", err)
		os.Exit(1)
	}
}
