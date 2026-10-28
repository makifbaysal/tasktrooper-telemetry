// Command loadtest-ctl claims queued load test runs, starts loadtest-runner per region (local subprocess or
// Kubernetes Job), merges results, handles aborts and verifies target domains (ADR-015, contracts §14).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/config"
	"github.com/tasktrooper/telemetry/services/pkg/logx"

	"github.com/tasktrooper/telemetry/services/loadtest/internal/ctl"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/obs"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/store"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest-ctl:", err)
		os.Exit(1)
	}
}

func run() error {
	common := config.Load()
	log := logx.New("loadtest-ctl", common.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	host, _ := os.Hostname()
	limits := ctl.Limits{
		MaxVUs:         config.Int("TT_LOADTEST_MAX_VUS", 500),
		MaxDuration:    config.Duration("TT_LOADTEST_MAX_DURATION", time.Hour),
		MonthlyMinutes: float64(config.Int("TT_LOADTEST_MONTHLY_VU_MINUTES", 0)),
		AllowLocal:     config.Bool("TT_LOADTEST_ALLOW_LOCAL", false),
	}
	// shared runner env: what every runner needs to reach Postgres and ingest
	runnerEnv := map[string]string{
		"TT_POSTGRES_URL":          common.PostgresURL,
		"TT_LOG_LEVEL":             common.LogLevel,
		"TT_LOADTEST_INGEST_URL":   os.Getenv("TT_LOADTEST_INGEST_URL"),
		"TT_DSN":                   os.Getenv("TT_DSN"),
		"TT_INTERNAL_TOKEN":        os.Getenv("TT_INTERNAL_TOKEN"),
		"TT_LOADTEST_MAX_DURATION": limits.MaxDuration.String(),
		"TT_LOADTEST_TICK":         os.Getenv("TT_LOADTEST_TICK"),
	}
	for k, v := range runnerEnv {
		if v == "" {
			delete(runnerEnv, k)
		}
	}
	var ex ctl.Executor
	switch kind := config.String("TT_LOADTEST_EXECUTOR", "local"); kind {
	case "local":
		bin, err := ctl.FindRunner(os.Getenv("TT_LOADTEST_RUNNER_BIN"))
		if err != nil {
			return fmt.Errorf("local executor: loadtest-runner not found (TT_LOADTEST_RUNNER_BIN): %w", err)
		}
		ex = &ctl.LocalExecutor{Bin: bin, Env: runnerEnv}
	case "k8s":
		// secrets (TT_POSTGRES_URL, TT_INTERNAL_TOKEN) come from TT_LOADTEST_K8S_SECRET, never as plain env
		plain := map[string]string{}
		for k, v := range runnerEnv {
			if k != "TT_POSTGRES_URL" && k != "TT_INTERNAL_TOKEN" && k != "TT_DSN" {
				plain[k] = v
			}
		}
		k, err := ctl.NewK8sExecutor(ctl.K8sConfig{
			APIServer: os.Getenv("TT_LOADTEST_K8S_API"), Namespace: os.Getenv("TT_LOADTEST_K8S_NAMESPACE"),
			Image: os.Getenv("TT_LOADTEST_K8S_IMAGE"), ServiceAccount: os.Getenv("TT_LOADTEST_K8S_SERVICE_ACCOUNT"),
			SecretName: os.Getenv("TT_LOADTEST_K8S_SECRET"), RegionLabel: os.Getenv("TT_LOADTEST_K8S_REGION_LABEL"),
			CPU: os.Getenv("TT_LOADTEST_K8S_CPU"), Memory: os.Getenv("TT_LOADTEST_K8S_MEMORY"), Env: plain,
		})
		if err != nil {
			return err
		}
		ex = k
	default:
		return fmt.Errorf("TT_LOADTEST_EXECUTOR must be local|k8s, got %q", kind)
	}

	st, err := store.Open(ctx, common.PostgresURL)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer st.Close()

	reg := obs.New()
	c := ctl.New(ctl.Config{
		Instance:       config.String("TT_LOADTEST_INSTANCE", host),
		Limits:         limits,
		DefaultRegions: config.List("TT_LOADTEST_REGIONS", ""),
		Poll:           config.Duration("TT_LOADTEST_POLL", time.Second),
		VerifyEvery:    config.Duration("TT_LOADTEST_VERIFY_INTERVAL", 10*time.Second),
		MaxConcurrent:  config.Int("TT_LOADTEST_MAX_CONCURRENT", 4),
		Projects:       projects(),
	}, st, ex, ctl.NewVerifier(), log, reg)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.Handle("GET /metrics", reg)
	srv := &http.Server{Addr: config.String("TT_LOADTEST_ADDR", ":8097"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "err", err)
			stop()
		}
	}()
	log.Info("loadtest-ctl starting", "version", version, "addr", srv.Addr, "max_vus", limits.MaxVUs,
		"max_duration", limits.MaxDuration.String(), "monthly_vu_minutes", limits.MonthlyMinutes, "allow_local", limits.AllowLocal)
	err = c.Run(ctx)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	return err
}

// projects parses TT_LOADTEST_PROJECTS (comma separated project ids; empty = all projects).
func projects() []int64 {
	var out []int64
	for _, s := range config.List("TT_LOADTEST_PROJECTS", "") {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			out = append(out, id)
		}
	}
	return out
}
