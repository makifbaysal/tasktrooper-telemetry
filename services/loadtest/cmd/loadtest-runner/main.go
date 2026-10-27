// Command loadtest-runner executes one region shard of a load test run (started by loadtest-ctl).
// Env: TT_POSTGRES_URL, TT_LOADTEST_RUN_ID, TT_LOADTEST_REGION, TT_LOADTEST_SHARE, TT_LOADTEST_VU_CAP,
// TT_LOADTEST_INGEST_URL | TT_DSN, TT_INTERNAL_TOKEN.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/config"
	"github.com/tasktrooper/telemetry/services/pkg/logx"

	"github.com/tasktrooper/telemetry/services/loadtest/internal/runner"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/store"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest-runner:", err)
		os.Exit(1)
	}
}

func run() error {
	common := config.Load()
	log := logx.New("loadtest-runner", common.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	id := int64(config.Int("TT_LOADTEST_RUN_ID", 0))
	if id <= 0 {
		return fmt.Errorf("TT_LOADTEST_RUN_ID is required")
	}
	share := 1.0
	if v, err := parseFloat(os.Getenv("TT_LOADTEST_SHARE")); err == nil && v > 0 && v <= 1 {
		share = v
	}
	st, err := store.Open(ctx, common.PostgresURL)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer st.Close()
	return runner.Run(ctx, st, runner.Config{
		RunID:         id,
		Region:        config.String("TT_LOADTEST_REGION", "local"),
		Share:         share,
		VUCap:         config.Int("TT_LOADTEST_VU_CAP", 0),
		Tick:          config.Duration("TT_LOADTEST_TICK", 5*time.Second),
		MaxDuration:   config.Duration("TT_LOADTEST_MAX_DURATION", time.Hour),
		IngestURL:     os.Getenv("TT_LOADTEST_INGEST_URL"),
		DSN:           os.Getenv("TT_DSN"),
		InternalToken: os.Getenv("TT_INTERNAL_TOKEN"),
		K6Binary:      os.Getenv("TT_LOADTEST_K6_BIN"),
		Version:       version,
	}, log)
}

func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscan(s, &f)
	return f, err
}
