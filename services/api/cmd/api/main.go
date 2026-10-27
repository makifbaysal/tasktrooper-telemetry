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
	"github.com/tasktrooper/telemetry/services/api/internal/slack"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/api/internal/symbolicate"
	"github.com/tasktrooper/telemetry/services/pkg/config"
	"github.com/tasktrooper/telemetry/services/pkg/githubapp"
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
		DevLogin:     config.Bool("TT_AUTH_DEV", false),
		SessionTTL:   config.Duration("TT_SESSION_TTL", 7*24*time.Hour),
		WebURL:       config.String("TT_WEB_URL", "http://localhost:5173"),
		CookieSecure: config.Bool("TT_COOKIE_SECURE", false),
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
	if issuer := config.String("TT_OIDC_ISSUER", ""); issuer != "" {
		srv.OIDC = &auth.OIDC{Issuer: issuer, ClientID: config.String("TT_OIDC_CLIENT_ID", ""),
			ClientSecret: config.String("TT_OIDC_CLIENT_SECRET", ""),
			RedirectURL:  config.String("TT_OIDC_REDIRECT_URL", "http://localhost:8080/api/v1/auth/callback"),
			Scopes:       config.List("TT_OIDC_SCOPES", "openid,email,profile"),
			HTTPClient:   &http.Client{Timeout: 10 * time.Second}}
		if srv.OIDC.ClientID == "" {
			return errors.New("TT_OIDC_ISSUER is set but TT_OIDC_CLIENT_ID is empty")
		}
	}
	srv.StateKey = key
	srv.PublicAPIURL = config.String("TT_PUBLIC_API_URL", "http://localhost:8080")
	if id := config.String("TT_SLACK_CLIENT_ID", ""); id != "" {
		srv.Slack = &server.SlackApp{ClientID: id, ClientSecret: config.String("TT_SLACK_CLIENT_SECRET", ""),
			SigningSecret: config.String("TT_SLACK_SIGNING_SECRET", ""),
			API:           &slack.Client{BaseURL: config.String("TT_SLACK_API_URL", slack.DefaultAPIURL), HTTP: &http.Client{Timeout: 10 * time.Second}}}
		if srv.Slack.ClientSecret == "" || srv.Slack.SigningSecret == "" {
			return errors.New("TT_SLACK_CLIENT_ID is set but TT_SLACK_CLIENT_SECRET / TT_SLACK_SIGNING_SECRET is empty")
		}
	}
	switch gh, err := githubapp.FromEnv(ctx); {
	case err == nil:
		srv.GitHub = &server.GitHubApp{Client: gh, Slug: config.String("TT_GITHUB_APP_SLUG", ""), WebURL: config.String("TT_GITHUB_URL", "https://github.com")}
		if srv.GitHub.Slug == "" {
			return errors.New("TT_GITHUB_APP_ID is set but TT_GITHUB_APP_SLUG is empty")
		}
	case !errors.Is(err, githubapp.ErrNotConfigured):
		return err
	}
	if srv.DevLogin {
		log.Warn("TT_AUTH_DEV=true: POST /api/v1/auth/dev-login signs anyone in by email; never enable it in production")
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
