// Command notifier delivers the notifications outbox (Slack App + webhooks, PagerDuty, Opsgenie, webhook, e-mail, Teams).
// Any number of replicas may run: rows are claimed with FOR UPDATE SKIP LOCKED.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/config"
	"github.com/tasktrooper/telemetry/services/pkg/logx"
	"github.com/tasktrooper/telemetry/services/pkg/secrets"

	"github.com/tasktrooper/telemetry/services/alerting/internal/conn"
	"github.com/tasktrooper/telemetry/services/alerting/internal/notify"
	"github.com/tasktrooper/telemetry/services/alerting/internal/obs"
	"github.com/tasktrooper/telemetry/services/alerting/internal/store"
)

func main() {
	common := config.Load()
	log := logx.New("notifier", common.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pg, err := conn.Postgres(ctx, common.PostgresURL)
	if err != nil {
		log.Error("postgres", "err", err)
		os.Exit(1)
	}
	defer pg.Close()

	key, err := secrets.Key()
	if err != nil {
		log.Error("secrets key", "err", err)
		os.Exit(1)
	}
	reg := obs.NewRegistry()
	reg.Describe("tt_notifier_deliveries_total", "counter", "notification deliveries by outcome and channel type")
	d := notify.NewDeliverer(notify.SMTPConfig{
		Addr:     config.String("TT_NOTIFIER_SMTP_ADDR", ""),
		User:     config.String("TT_NOTIFIER_SMTP_USER", ""),
		Password: config.String("TT_NOTIFIER_SMTP_PASSWORD", ""),
		From:     config.String("TT_NOTIFIER_SMTP_FROM", "alerts@tasktrooper.local"),
	})
	res := notify.NewSecretResolver(&store.Store{DB: pg}, key)
	// Slack App targets slack:<channel id>#<name> (ADR-017): org bot token from integrations(provider='slack').
	d.Slack = &notify.SlackApp{
		Base:         config.String("TT_SLACK_API_URL", notify.SlackAPIBase),
		Store:        &notify.PGSlackStore{DB: pg, Secrets: res},
		PublicAppURL: config.String("TT_PUBLIC_APP_URL", config.String("TT_PUBLIC_URL", "")),
	}
	w := &notify.Worker{DB: pg, D: d, Log: log, Reg: reg, Secrets: res, Cfg: notify.WorkerConfig{
		Poll:        config.Duration("TT_NOTIFIER_POLL", 2*time.Second),
		Batch:       config.Int("TT_NOTIFIER_BATCH", 50),
		MaxAttempts: config.Int("TT_NOTIFIER_MAX_ATTEMPTS", 10),
		Concurrency: config.Int("TT_NOTIFIER_CONCURRENCY", 8),
	}}
	addr := config.String("TT_NOTIFIER_ADDR", ":8091")
	go obs.Serve(ctx, addr, reg, func(c context.Context) error { return pg.Ping(c) }, log)
	log.Info("notifier started", "addr", addr)
	if err := w.Run(ctx); err != nil {
		log.Error("notifier stopped", "err", err)
		os.Exit(1)
	}
	log.Info("notifier stopped")
}
