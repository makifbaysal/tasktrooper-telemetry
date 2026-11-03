// Command ingest-writer consumes the ingest topics and batch-inserts rows into ClickHouse.
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

const service = "tt-ingest-writer"

// version is set at build time (-ldflags "-X main.version=...", Dockerfile ARG VERSION).
var version = "dev"

func main() {
	cfg := app.WriterFromEnv()
	log := logx.New("ingest-writer", cfg.Common.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Self-telemetry (ADR-027): only with TROOPER_DSN; otherwise nothing is installed.
	shutdown, enabled, err := selfobs.Start(ctx, service, version)
	if err != nil {
		log.Warn("self-telemetry disabled", "err", err)
	}
	if enabled {
		log = slog.New(selfobs.LogHandler(log.Handler(), service))
		cfg.Telemetry = true
	}
	err = app.RunWriter(ctx, cfg, log)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = shutdown(sctx)
	cancel()
	if err != nil {
		log.Error("ingest-writer stopped", "err", err)
		os.Exit(1)
	}
}
