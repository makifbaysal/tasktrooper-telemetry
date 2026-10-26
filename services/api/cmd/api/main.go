// Command api serves the dashboard query API (/api/v1) over ClickHouse and Postgres.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/cloudcheck"
	"github.com/tasktrooper/telemetry/services/api/internal/conntest"
	"github.com/tasktrooper/telemetry/services/api/internal/server"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/api/internal/symbolicate"
	"github.com/tasktrooper/telemetry/services/pkg/config"
	"github.com/tasktrooper/telemetry/services/pkg/logx"
	"github.com/tasktrooper/telemetry/services/pkg/secrets"
)

func main() {
	cfg := config.Load()
	log := logx.New("api", cfg.LogLevel)
	if err := run(cfg, log); err != nil {
		log.Error("api stopped", "err", err)
		os.Exit(1)
	}
}

func run(cfg config.Common, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.PostgresURL)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	opts, err := clickhouse.ParseDSN(cfg.ClickHouseURL)
	if err != nil {
		return fmt.Errorf("clickhouse dsn: %w", err)
	}
	opts.MaxOpenConns = config.Int("TT_API_CH_MAX_CONNS", 20)
	ch, err := clickhouse.Open(opts)
	if err != nil {
		return fmt.Errorf("clickhouse: %w", err)
	}
	defer ch.Close()

	key, err := secrets.Key()
	if err != nil {
		return err
	}
	st := store.New(ch, pool)
	st.SecretsKey = key
	srv := &server.Server{
		Store:        st,
		CloudTest:    cloudcheck.Run,
		Symbolicator: symbolicate.New(config.String("TT_SYMBOLICATOR_URL", "http://localhost:8095")),
		Auth:         auth.NewPG(pool),
		Log:          log,
		Origins:      config.List("TT_API_CORS_ORIGINS", "http://localhost:*,http://127.0.0.1:*"),
		Test:         conntest.Run,
		Ready: func(ctx context.Context) error {
			if err := pool.Ping(ctx); err != nil {
				return fmt.Errorf("postgres: %w", err)
			}
			if err := ch.Ping(ctx); err != nil {
				return fmt.Errorf("clickhouse: %w", err)
			}
			return nil
		},
	}
	hs := &http.Server{
		Addr:              config.String("TT_API_ADDR", ":8080"),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("api listening", "addr", hs.Addr)
		errc <- hs.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return hs.Shutdown(shutdown)
}
