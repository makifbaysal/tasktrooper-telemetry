// Command alert-engine evaluates monitors and KPI burn-rate rules, manages incidents and fills the
// notification outbox. Only the advisory-lock leader (per shard) evaluates; other replicas stand by.
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/config"
	"github.com/tasktrooper/telemetry/services/pkg/logx"

	"github.com/tasktrooper/telemetry/services/alerting/internal/conn"
	"github.com/tasktrooper/telemetry/services/alerting/internal/engine"
	"github.com/tasktrooper/telemetry/services/alerting/internal/leader"
	"github.com/tasktrooper/telemetry/services/alerting/internal/obs"
	"github.com/tasktrooper/telemetry/services/alerting/internal/store"
	"github.com/tasktrooper/telemetry/services/alerting/internal/stream"
)

func main() {
	common := config.Load()
	log := logx.New("alert-engine", common.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pg, err := conn.Postgres(ctx, common.PostgresURL)
	if err != nil {
		log.Error("postgres", "err", err)
		os.Exit(1)
	}
	defer pg.Close()
	ch, err := conn.ClickHouse(ctx, common.ClickHouseURL)
	if err != nil {
		log.Error("clickhouse", "err", err)
		os.Exit(1)
	}
	defer ch.Close()

	var projects []int64
	for _, s := range config.List("TT_ALERT_PROJECTS", "") {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			projects = append(projects, id)
		}
	}
	reg := obs.NewRegistry()
	el := &leader.Elector{
		Pool:           pg,
		Base:           int64(config.Int("TT_ALERT_LOCK_KEY", int(leader.DefaultBase))),
		Shards:         config.Int("TT_ALERT_SHARDS", 1),
		MaxPerInstance: config.Int("TT_ALERT_MAX_SHARDS", 0),
	}
	eng := engine.New(engine.Config{
		Interval:    config.Duration("TT_ALERT_INTERVAL", 30*time.Second),
		Refresh:     config.Duration("TT_ALERT_REFRESH", 5*time.Minute),
		EvalTimeout: config.Duration("TT_ALERT_EVAL_TIMEOUT", 10*time.Second),
		Workers:     config.Int("TT_ALERT_WORKERS", 8),
		SLIBackfill: config.Duration("TT_ALERT_SLI_BACKFILL", 6*time.Hour),
		PublicURL:   config.String("TT_PUBLIC_URL", "http://localhost:5173"),
		ProjectIDs:  projects,
	}, log, &store.Store{DB: pg}, ch, el, reg)

	addr := config.String("TT_ALERT_ADDR", ":8090")
	go obs.Serve(ctx, addr, reg, func(c context.Context) error {
		return errors.Join(pg.Ping(c), ch.Ping(c))
	}, log)
	if config.Bool("TT_ALERT_STREAM", false) {
		go func() {
			_ = stream.Run(ctx, stream.Config{Brokers: common.KafkaBrokers, Group: config.String("TT_ALERT_STREAM_GROUP", stream.Group)}, eng, reg, log)
		}()
	}
	log.Info("alert-engine started", "addr", addr, "shards", el.Shards, "stream", config.Bool("TT_ALERT_STREAM", false))
	if err := eng.Run(ctx); err != nil {
		log.Error("engine stopped", "err", err)
		os.Exit(1)
	}
	log.Info("alert-engine stopped")
}
