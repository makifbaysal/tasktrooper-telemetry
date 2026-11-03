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
	"github.com/tasktrooper/telemetry/services/api/internal/ai"
	"github.com/tasktrooper/telemetry/services/api/internal/assistant"
	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/blob"
	"github.com/tasktrooper/telemetry/services/api/internal/cloudcheck"
	"github.com/tasktrooper/telemetry/services/api/internal/conntest"
	"github.com/tasktrooper/telemetry/services/api/internal/selftel"
	"github.com/tasktrooper/telemetry/services/api/internal/server"
	"github.com/tasktrooper/telemetry/services/api/internal/slack"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/api/internal/symbolicate"
	"github.com/tasktrooper/telemetry/services/pkg/aiquota"
	"github.com/tasktrooper/telemetry/services/pkg/config"
	"github.com/tasktrooper/telemetry/services/pkg/githubapp"
	"github.com/tasktrooper/telemetry/services/pkg/llmapi"
	"github.com/tasktrooper/telemetry/services/pkg/logx"
	"github.com/tasktrooper/telemetry/services/pkg/secrets"
)

// version is set at build time (-ldflags "-X main.version=..."); TROOPER_VERSION overrides it for telemetry.
var version = ""

func main() {
	if len(os.Args) > 1 && os.Args[1] == "bootstrap" { // ADR-028: org / project / ingest key without seed SQL
		os.Exit(bootstrapMain(os.Args[2:], os.Stdout, os.Stderr))
	}
	cfg := config.Load()
	log := logx.New("api", cfg.LogLevel)
	// Self telemetry (ADR-027): a no-op unless TROOPER_DSN is set.
	flush := selftel.Start(context.Background(), version, log)
	log = selftel.Logger(log)
	err := run(cfg, log)
	if err != nil {
		log.Error("api stopped", "err", err)
	}
	flush()
	if err != nil {
		os.Exit(1)
	}
}

func run(cfg config.Common, log *slog.Logger) error {
	if selftel.Enabled() { // outgoing calls to customer clouds / AI provider get client spans (ADR-027)
		ai.HTTPTransport = selftel.LLMTransport()
		cloudcheck.HTTPClient = selftel.Client(http.DefaultClient, "")
		conntest.HTTPClient = selftel.Client(http.DefaultClient, "")
	}
	// Every LLM call goes through one OpenAI-compatible endpoint (ADR-034): TT_ASSISTANT_API_URL / _API_KEY / _MODEL.
	llmCfg := llmapi.FromEnv()
	summarizer := ai.New(llmCfg, nil)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pcfg, err := pgxpool.ParseConfig(cfg.PostgresURL)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	if selftel.Enabled() {
		pcfg.ConnConfig.Tracer = selftel.PGTracer()
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
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
	traced := ch // ClickHouse client spans for the store; Ready pings the raw conn (untraced)
	if selftel.Enabled() {
		addr := ""
		if len(opts.Addr) > 0 {
			addr = opts.Addr[0]
		}
		traced = selftel.ClickHouse(ch, opts.Auth.Database, addr)
	}

	key, err := secrets.Key()
	if err != nil {
		return err
	}
	st := store.New(traced, pool)
	st.SecretsKey = key
	if st.AIQuota, err = aiquota.FromEnv(); err != nil {
		return err
	}
	sym := symbolicate.New(config.String("TT_SYMBOLICATOR_URL", "http://localhost:8095"))
	if sym != nil {
		sym.HTTP = selftel.Client(sym.HTTP, "tt-symbolicator")
		sym.InternalToken = config.String("TT_INTERNAL_TOKEN", "") // dashboard sessions have no bearer key (contracts §2)
	}
	srv := &server.Server{
		Store:        st,
		AI:           summarizer,
		CloudTest:    cloudcheck.Run,
		Symbolicator: sym,
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
			HTTPClient:   selftel.Client(&http.Client{Timeout: 10 * time.Second}, "oidc")}
		if srv.OIDC.ClientID == "" {
			return errors.New("TT_OIDC_ISSUER is set but TT_OIDC_CLIENT_ID is empty")
		}
	}
	// Dashboard assistant (ADR-033): an OpenAI-compatible chat completions model (default URL: Meta Muse Spark) when
	// TT_ASSISTANT_API_KEY and TT_ASSISTANT_MODEL are both set, else the offline mock.
	srv.Assistant = &server.AssistantConfig{
		Provider: assistant.NewOpenAI(llmCfg.URL, llmCfg.Key, llmCfg.Model,
			&http.Client{Timeout: 5 * time.Minute, Transport: selftel.LLMTransport()}),
		MaxSteps:   config.Int("TT_ASSISTANT_MAX_STEPS", 12),
		MaxTokens:  int64(config.Int("TT_ASSISTANT_MAX_TOKENS", 4096)),
		AllowWrite: config.Bool("TT_ASSISTANT_ALLOW_WRITE", true),
	}
	if srv.Assistant.Provider == nil {
		log.Info("assistant uses the offline mock (set TT_ASSISTANT_API_KEY and TT_ASSISTANT_MODEL for a model)")
	}
	// S3 client spans (peer.service s3); nil (SDK default client) when self telemetry is off.
	if b := blob.FromEnv(selftel.Client(nil, "s3")); b != nil {
		srv.Replays = b
	}
	srv.StateKey = key
	srv.PublicAPIURL = config.String("TT_PUBLIC_API_URL", "http://localhost:8080")
	srv.PublicWebhookURL = config.String("TT_PUBLIC_WEBHOOK_URL", "http://localhost:8096")
	srv.PublicIngestURL = config.String("TT_PUBLIC_INGEST_URL", store.DefaultPublicIngestURL)
	if _, err := store.DSN(srv.PublicIngestURL, "k", 1); err != nil {
		return fmt.Errorf("TT_PUBLIC_INGEST_URL: %w", err)
	}
	srv.MaxOrgsPerUser = config.Int("TT_MAX_ORGS_PER_USER", store.DefaultMaxOrgsPerUser)
	if id := config.String("TT_SLACK_CLIENT_ID", ""); id != "" {
		srv.Slack = &server.SlackApp{ClientID: id, ClientSecret: config.String("TT_SLACK_CLIENT_SECRET", ""),
			SigningSecret: config.String("TT_SLACK_SIGNING_SECRET", ""),
			API:           &slack.Client{BaseURL: config.String("TT_SLACK_API_URL", slack.DefaultAPIURL), HTTP: selftel.Client(&http.Client{Timeout: 10 * time.Second}, "slack")}}
		if srv.Slack.ClientSecret == "" || srv.Slack.SigningSecret == "" {
			return errors.New("TT_SLACK_CLIENT_ID is set but TT_SLACK_CLIENT_SECRET / TT_SLACK_SIGNING_SECRET is empty")
		}
	}
	switch gh, err := githubapp.FromEnv(ctx); {
	case err == nil:
		gh.HTTP = selftel.Client(gh.HTTP, "github")
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
