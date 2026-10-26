// Command trooper-agent collects database and message broker metrics and ships them to
// TaskTrooper as OTLP metrics (contracts §5).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/config"
	"github.com/tasktrooper/telemetry/services/pkg/logx"

	"github.com/tasktrooper/telemetry/services/collector/internal/agent"
	ccfg "github.com/tasktrooper/telemetry/services/collector/internal/config"
	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/scrape"
)

var version = "dev"

type flags struct {
	configPath   string
	apiURL       string
	apiKey       string
	dsn          string
	modes        string
	hostMetrics  bool
	hostDiskPath string
	pollEvery    time.Duration
	once         bool
	onceGap      time.Duration
	logLevel     string
}

func parseFlags(args []string) (flags, error) {
	var f flags
	fs := flag.NewFlagSet("trooper-agent", flag.ContinueOnError)
	fs.StringVar(&f.configPath, "config", config.String("TT_COLLECTOR_CONFIG", ""), "static YAML target list (env TT_COLLECTOR_CONFIG)")
	fs.StringVar(&f.apiURL, "api-url", config.String("TT_API_URL", ""), "pull targets from {api-url}/api/v1/internal/collector-config (env TT_API_URL)")
	fs.StringVar(&f.apiKey, "api-key", config.String("TT_API_KEY", ""), "api key for the config endpoint (env TT_API_KEY)")
	fs.StringVar(&f.dsn, "dsn", config.String("TT_DSN", ""), "ingest DSN, e.g. http://tt_dev_ingest_key@localhost:4318/1 (env TT_DSN)")
	fs.StringVar(&f.modes, "modes", config.String("TT_COLLECTOR_MODES", ""), "only scrape targets whose collector is in this list (agent,direct,cloud); empty = all")
	fs.BoolVar(&f.hostMetrics, "host-metrics", config.Bool("TT_COLLECTOR_HOST_METRICS", false), "report this host's CPU/memory/disk for agent-mode databases")
	fs.StringVar(&f.hostDiskPath, "host-disk-path", config.String("TT_COLLECTOR_HOST_DISK_PATH", "/"), "filesystem whose usage is trooper.db.disk")
	fs.DurationVar(&f.pollEvery, "poll", config.Duration("TT_COLLECTOR_POLL", 60*time.Second), "remote config poll interval")
	fs.BoolVar(&f.once, "once", false, "scrape every target twice (--once-gap apart), print the samples as JSON, flush and exit")
	fs.DurationVar(&f.onceGap, "once-gap", 5*time.Second, "gap between the two scrapes of --once (rates need two readings)")
	fs.StringVar(&f.logLevel, "log-level", config.String("TT_LOG_LEVEL", "info"), "debug|info|warn|error")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if f.configPath == "" && f.apiURL == "" {
		return f, errors.New("no targets: set --config (static YAML) and/or --api-url + --api-key")
	}
	if f.apiURL != "" && f.apiKey == "" {
		return f, errors.New("--api-url needs --api-key (TT_API_KEY)")
	}
	if f.dsn == "" && !f.once {
		return f, errors.New("--dsn (TT_DSN) is required")
	}
	return f, nil
}

func main() {
	f, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "trooper-agent:", err)
		os.Exit(2)
	}
	log := logx.New("trooper-agent", f.logLevel)
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, f, log); err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, f flags, log *slog.Logger) error {
	var static *ccfg.Config
	if f.configPath != "" {
		c, err := ccfg.LoadFile(f.configPath)
		if err != nil {
			return err
		}
		static = c
	}
	fetcher := ccfg.Fetcher{APIURL: f.apiURL, APIKey: f.apiKey}
	load := func(ctx context.Context) (*ccfg.Config, error) {
		if f.apiURL == "" {
			return ccfg.Merge(static, nil), nil
		}
		remote, err := fetcher.Fetch(ctx)
		if err != nil {
			return nil, err
		}
		return ccfg.Merge(static, remote), nil
	}
	opts := agent.Options{Host: scrape.HostOptions{Enabled: f.hostMetrics, DiskPath: f.hostDiskPath}}
	for _, m := range strings.Split(f.modes, ",") {
		if m = strings.TrimSpace(m); m != "" {
			opts.Modes = append(opts.Modes, m)
		}
	}

	cfg, err := load(ctx)
	if err != nil {
		if static == nil {
			log.Warn("remote config unavailable, waiting for the next poll", "err", err)
		} else {
			log.Warn("remote config unavailable, using static config only", "err", err)
		}
		cfg = ccfg.Merge(static, nil)
	}

	var em *emit.Emitter
	if f.dsn != "" {
		d, err := ccfg.ParseIngestDSN(f.dsn)
		if err != nil {
			return err
		}
		em, err = emit.New(ctx, emit.Options{MetricsURL: d.MetricsURL, Key: d.Key, Interval: cfg.Interval.D(), Version: version, Logger: log})
		if err != nil {
			return err
		}
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := em.Shutdown(sctx); err != nil {
				log.Warn("flush on shutdown failed", "err", err)
			}
		}()
	}

	if f.once {
		return runOnce(ctx, cfg, opts, em, f.onceGap, log)
	}

	a := agent.New(em, log)
	a.Apply(ctx, agent.Targets(cfg, opts, log))
	log.Info("trooper-agent started", "version", version, "targets", len(a.IDs()), "remote", f.apiURL != "")
	defer a.Stop()

	if f.apiURL == "" {
		<-ctx.Done()
		return nil
	}
	tk := time.NewTicker(f.pollEvery)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tk.C:
			c, err := load(ctx)
			if err != nil {
				log.Warn("remote config poll failed, keeping current targets", "err", err)
				continue
			}
			a.Apply(ctx, agent.Targets(c, opts, log))
		}
	}
}

// recorder keeps the last result per target (for --once output) and forwards to the emitter.
type recorder struct {
	next *emit.Emitter
	last map[string]emit.Result
}

func (r *recorder) Publish(ctx context.Context, target string, res emit.Result) {
	r.last[target] = res
	if r.next != nil {
		r.next.Publish(ctx, target, res)
	}
}

func (r *recorder) Remove(target string) {
	delete(r.last, target)
	if r.next != nil {
		r.next.Remove(target)
	}
}

func runOnce(ctx context.Context, cfg *ccfg.Config, opts agent.Options, em *emit.Emitter, gap time.Duration, log *slog.Logger) error {
	rec := &recorder{next: em, last: map[string]emit.Result{}}
	targets := agent.Targets(cfg, opts, log)
	runners := make([]*agent.Runner, len(targets))
	for i, t := range targets {
		runners[i] = &agent.Runner{T: t, Pub: rec, Log: log}
		defer runners[i].Close()
		runners[i].Once(ctx)
	}
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(gap):
	}
	failed := 0
	for _, r := range runners {
		if !r.Once(ctx) {
			failed++
		}
	}
	if em != nil {
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := em.Flush(fctx); err != nil {
			return fmt.Errorf("flush: %w", err)
		}
	}
	enc := json.NewEncoder(os.Stdout)
	for _, t := range targets {
		res := rec.last[t.ID]
		for _, kind := range []struct {
			typ string
			s   []emit.Sample
		}{{"gauge", res.Gauges}, {"delta_sum", res.Counts}} {
			for _, s := range kind.s {
				attrs := map[string]string{}
				for _, kv := range s.Attrs.ToSlice() {
					attrs[string(kv.Key)] = kv.Value.Emit()
				}
				_ = enc.Encode(map[string]any{"target": t.ID, "type": kind.typ, "metric": s.Name, "value": s.Value, "attributes": attrs})
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d targets failed", failed, len(targets))
	}
	return nil
}
