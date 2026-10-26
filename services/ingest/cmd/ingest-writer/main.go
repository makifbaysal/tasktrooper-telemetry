// Command ingest-writer consumes the ingest topics and batch-inserts rows into ClickHouse.
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
	cfg := app.WriterFromEnv()
	log := logx.New("ingest-writer", cfg.Common.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.RunWriter(ctx, cfg, log); err != nil {
		log.Error("ingest-writer stopped", "err", err)
		os.Exit(1)
	}
}
