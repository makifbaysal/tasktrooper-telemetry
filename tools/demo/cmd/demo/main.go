// Command demo is the TaskTrooper demo environment: it reproduces the dashboard's mock world with
// real services (Go SDK), real databases / brokers (ttdemo-* containers), real RUM batches and the
// real collector, all flowing through the real ingest pipeline.
//
//	demo up     platform (if not running) + seed + run        — one command
//	demo seed   containers, schemas, topics, api metadata (idempotent)
//	demo run    services, client traffic, collector until Ctrl-C
//	demo down   remove ttdemo containers and demo topics
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tasktrooper/telemetry/tools/demo/internal/infra"
	"github.com/tasktrooper/telemetry/tools/demo/internal/infrasim"
	"github.com/tasktrooper/telemetry/tools/demo/internal/orch"
	"github.com/tasktrooper/telemetry/tools/demo/internal/seed"
	"github.com/tasktrooper/telemetry/tools/demo/internal/sim"
	"github.com/tasktrooper/telemetry/tools/demo/internal/svc"
	"github.com/tasktrooper/telemetry/tools/demo/internal/world"
)

type opts struct {
	api, apiKey, dsn, kafka, postgres string
	rps                               float64
	scenarios                         string
	incidentAfter, duration, recoCyc  time.Duration
	keep, debug, infraSim             bool
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cmd == "svc" { // internal: one simulated service instance
		fs := flag.NewFlagSet("svc", flag.ExitOnError)
		inst := fs.Int("instance", 0, "")
		port := fs.Int("port", 0, "")
		_ = fs.Parse(args[min(1, len(args)):])
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "demo svc <name>")
			os.Exit(2)
		}
		if err := svc.Run(ctx, args[0], *inst, *port); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	o := opts{}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.StringVar(&o.api, "api", env("TT_API_URL", world.DefaultAPI), "api base URL")
	fs.StringVar(&o.apiKey, "api-key", env("TT_API_KEY", world.DefaultAPIKey), "api key")
	fs.StringVar(&o.dsn, "dsn", env("TROOPER_DSN", world.DefaultIngestDSN), "ingest DSN for the SDKs, RUM and the collector")
	fs.StringVar(&o.kafka, "kafka", env("TT_KAFKA_BROKERS", world.DefaultKafka), "Redpanda bootstrap brokers")
	fs.StringVar(&o.postgres, "postgres", env("TT_POSTGRES_URL", world.DefaultPostgres), "platform Postgres (notification channels, service metadata)")
	fs.Float64Var(&o.rps, "rps", 20, "≈ client requests per second into the backend (internal fan-out adds more)")
	fs.StringVar(&o.scenarios, "scenario", "all", "incident scenarios: all | none | comma list of "+strings.Join(world.ScenarioNames(), ","))
	fs.DurationVar(&o.incidentAfter, "incident-after", 0, "start healthy and switch the scenarios on after this delay (charts show the step)")
	fs.DurationVar(&o.duration, "duration", 0, "stop after this long (0 = until Ctrl-C)")
	fs.DurationVar(&o.recoCyc, "reco-trainer-every", 10*time.Minute, "reco-trainer batch cycle (hourly in the mock)")
	fs.BoolVar(&o.keep, "keep", false, "keep the ttdemo containers when stopping (up/run)")
	fs.BoolVar(&o.debug, "debug", false, "SDK debug output")
	fs.BoolVar(&o.infraSim, "infra-sim", true, "simulate the EKS cluster demo-eks, hosts, Warning events and 4 Lambda functions (trooper-agent host/cluster modes + AWS Lambda over OTLP)")
	_ = fs.Parse(args)

	var err error
	switch cmd {
	case "up":
		err = up(ctx, log, o)
	case "seed":
		err = doSeed(ctx, log, o)
	case "run":
		err = run(ctx, log, o, nil)
	case "down":
		err = down(log, o)
	case "scenarios":
		for _, s := range world.Scenarios {
			fmt.Printf("%-26s %s\n", s.Name, s.What)
		}
	default:
		usage()
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("demo failed", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: demo <command> [flags]
  up         start the platform services that are not running, seed, run (one command)
  seed       ttdemo containers + schemas + topics + api metadata (idempotent)
  run        simulated services, client traffic (RUM) and collector until Ctrl-C
  down       remove ttdemo containers and the demo topics / groups
  scenarios  list incident scenarios
flags: demo <command> -h
`)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func brokers(o opts) []string { return strings.Split(o.kafka, ",") }

// ---------------------------------------------------------------- commands

func doSeed(ctx context.Context, log *slog.Logger, o opts) error {
	if err := infra.Up(ctx, log); err != nil {
		return err
	}
	if err := infra.Load(ctx, log); err != nil {
		return err
	}
	if err := infra.Topics(ctx, brokers(o)); err != nil {
		return err
	}
	if err := infra.Rabbit(); err != nil {
		return fmt.Errorf("rabbitmq topology: %w", err)
	}
	if !orch.Healthy(o.api + "/healthz") {
		return fmt.Errorf("api not reachable at %s (start it, or use `demo up`)", o.api)
	}
	s := &seed.Seeder{API: o.api, Key: o.apiKey, PostgresURL: o.postgres, Log: log}
	return s.Run(ctx)
}

func down(log *slog.Logger, o opts) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := infra.DeleteTopics(ctx, brokers(o)); err != nil {
		log.Warn("delete topics", "err", err)
	}
	return infra.Down(ctx, log)
}

func up(ctx context.Context, log *slog.Logger, o opts) error {
	root, err := orch.Root()
	if err != nil {
		return err
	}
	sup, err := orch.NewSupervisor(root, log)
	if err != nil {
		return err
	}
	defer sup.StopAll()
	if err := sup.Platform(ctx); err != nil {
		return err
	}
	if err := doSeed(ctx, log, o); err != nil {
		return err
	}
	return run(ctx, log, o, sup)
}

// scenarioTimes resolves --scenario / --incident-after.
func scenarioTimes(o opts) (map[string]int64, error) {
	out := map[string]int64{}
	at := time.Now().Add(o.incidentAfter).UnixMilli()
	switch o.scenarios {
	case "none", "":
		return out, nil
	case "all":
		for _, n := range world.ScenarioNames() {
			out[n] = at
		}
		return out, nil
	}
	known := map[string]bool{}
	for _, n := range world.ScenarioNames() {
		known[n] = true
	}
	for _, n := range strings.Split(o.scenarios, ",") {
		n = strings.TrimSpace(n)
		if !known[n] {
			return nil, fmt.Errorf("unknown scenario %q (see `demo scenarios`)", n)
		}
		out[n] = at
	}
	return out, nil
}

func run(ctx context.Context, log *slog.Logger, o opts, sup *orch.Supervisor) error {
	root, err := orch.Root()
	if err != nil {
		return err
	}
	if sup == nil {
		if sup, err = orch.NewSupervisor(root, log); err != nil {
			return err
		}
		defer sup.StopAll()
	}
	if !o.keep {
		defer func() {
			log.Info("removing ttdemo containers (use --keep to keep them)")
			c, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_ = infra.Down(c, log)
		}()
	}
	if err := infra.Up(ctx, log); err != nil {
		return err
	}
	if err := infra.Topics(ctx, brokers(o)); err != nil {
		return err
	}
	if err := infra.Rabbit(); err != nil {
		return err
	}
	if !orch.Healthy(strings.TrimSuffix(ingestBase(o), "/") + "/healthz") {
		return fmt.Errorf("ingest-gw not reachable for DSN %s (use `demo up`)", o.dsn)
	}
	scen, err := scenarioTimes(o)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if o.duration > 0 {
		runCtx, cancel = context.WithTimeout(ctx, o.duration)
		defer cancel()
	}

	// reco-db: healthy index now; "migration 0042" drops it when the scenario switches on
	if err := infra.RecoIndex(ctx, true); err != nil {
		return fmt.Errorf("reco-db index: %w", err)
	}
	if at, ok := scen["reco-db-slow"]; ok {
		go func() {
			if d := time.Until(time.UnixMilli(at)); d > 0 {
				select {
				case <-runCtx.Done():
					return
				case <-time.After(d):
				}
			}
			log.Warn("scenario reco-db-slow: applying migration 0042 (DROP INDEX idx_reco_items_user_score)")
			if err := infra.RecoIndex(context.Background(), false); err != nil {
				log.Error("drop index", "err", err)
			}
		}()
	}

	rt := &world.Runtime{DSN: o.dsn, Peers: map[string]string{}, Kafka: brokers(o), Scenarios: scen, RPS: o.rps, Debug: o.debug}
	ext, err := sim.Externals(runCtx, rt)
	if err != nil {
		return err
	}
	for k, v := range ext {
		rt.Peers[k] = v
	}
	ports := map[string]int{}
	for _, s := range world.Services {
		if s.HTTP {
			p, err := orch.FreePort()
			if err != nil {
				return err
			}
			ports[s.Name] = p
			rt.Peers[s.Name] = fmt.Sprintf("http://127.0.0.1:%d", p)
		}
	}
	sink, err := sim.WebhookSink(runCtx, log, filepath.Join(sup.State, "logs", "notifications.log"))
	if err != nil {
		log.Warn("webhook sink disabled", "err", err)
	}

	// services: one OS process per instance (the SDK's providers are global per process)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	isSvc := func(p *orch.Proc) bool { return strings.HasPrefix(p.Name, "svc-") }
	defer sup.StopWhere(isSvc)
	for _, s := range world.Services {
		for i := 0; i < s.Instances; i++ {
			args := []string{"svc", s.Name, "--instance", strconv.Itoa(i), "--port", strconv.Itoa(ports[s.Name])}
			if _, err := sup.Start(fmt.Sprintf("svc-%s-%d", s.Name, i), root, self, args, []string{"DEMO_RUNTIME=" + rt.Encode()}); err != nil {
				return err
			}
		}
	}
	for name, p := range ports {
		if !waitTCP(runCtx, fmt.Sprintf("127.0.0.1:%d", p), 30*time.Second) {
			return fmt.Errorf("%s did not start (log: %s)", name, filepath.Join(sup.State, "logs", "svc-"+name+"-0.log"))
		}
	}
	log.Info("services up", "processes", len(world.Services)+3)

	// collector against the ttdemo containers
	bin, err := sup.Build(ctx, "collector", "./cmd/trooper-agent", "trooper-agent")
	if err != nil {
		return err
	}
	cfg, cenv := sup.CollectorConfig()
	if _, err := sup.Start("collector", filepath.Join(root, "services", "collector"), bin, []string{"--config", cfg, "--dsn", o.dsn},
		append(cenv, "TT_COLLECTOR_HOST_METRICS=false")); err != nil {
		return err
	}

	// uninstrumented consumers + client traffic
	sim.ExternalConsumers(runCtx, log, rt, o.recoCyc)
	sim.RabbitExternal(runCtx, log, o.rps)
	base, key, err := sim.ParseDSN(o.dsn)
	if err != nil {
		return err
	}
	tr := &sim.Traffic{RT: rt, Log: log, RPS: o.rps, IngestURL: base, IngestKey: key}
	go tr.Run(runCtx)
	if o.infraSim {
		go infrasim.New(base, key, rt, o.postgres, log).Run(runCtx)
	}

	active := []string{}
	for n := range scen {
		active = append(active, n)
	}
	log.Info("demo running — Ctrl-C to stop", "rps", o.rps, "scenarios", strings.Join(active, ","), "incident_after", o.incidentAfter,
		"logs", filepath.Join(sup.State, "logs"))
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-runCtx.Done():
			log.Info("stopping", "requests", tr.Requests.Load(), "rum_events", tr.RumSent.Load())
			return nil
		case <-tick.C:
			n := int64(0)
			if sink != nil {
				n = sink.Load()
			}
			log.Info("progress", "client_requests", tr.Requests.Load(), "rum_events", tr.RumSent.Load(), "rum_errors", tr.RumErr.Load(), "notifications", n)
			if p := sup.Exited(); p != nil {
				return fmt.Errorf("%s exited unexpectedly (log: %s)", p.Name, p.Log)
			}
		}
	}
}

func ingestBase(o opts) string {
	b, _, err := sim.ParseDSN(o.dsn)
	if err != nil {
		return o.dsn
	}
	return b
}

func waitTCP(ctx context.Context, addr string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}
