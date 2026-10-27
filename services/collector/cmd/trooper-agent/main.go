// Command trooper-agent collects database and message broker metrics (mode db, contracts §5), host /
// container / process metrics (mode host) and Kubernetes object state + Warning events (mode cluster,
// contracts §20) and ships them to TaskTrooper as OTLP metrics and logs.
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
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry/services/pkg/config"
	"github.com/tasktrooper/telemetry/services/pkg/logx"

	"github.com/tasktrooper/telemetry/services/collector/internal/agent"
	ccfg "github.com/tasktrooper/telemetry/services/collector/internal/config"
	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/infra"
	"github.com/tasktrooper/telemetry/services/collector/internal/scrape"
)

var version = "dev"

type flags struct {
	configPath   string
	apiURL       string
	apiKey       string
	dsn          string
	modes        string
	agentModes   []string
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
	agentModes := fs.String("agent-modes", config.String("TT_AGENT_MODES", "db"), "comma list of db, host, cluster (env TT_AGENT_MODES)")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	m, err := infra.ParseModes(*agentModes)
	if err != nil {
		return f, err
	}
	f.agentModes = m
	if f.has(infra.ModeDB) && f.configPath == "" && f.apiURL == "" {
		return f, errors.New("no targets: set --config (static YAML) and/or --api-url + --api-key, or run without the db mode (--agent-modes host,cluster)")
	}
	if f.apiURL != "" && f.apiKey == "" {
		return f, errors.New("--api-url needs --api-key (TT_API_KEY)")
	}
	if f.dsn == "" && !f.once {
		return f, errors.New("--dsn (TT_DSN) is required")
	}
	return f, nil
}

func (f flags) has(mode string) bool { return slices.Contains(f.agentModes, mode) }

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

// group is one emitter (one OTLP resource) and the targets publishing to it.
type group struct {
	name     string
	targets  []agent.Target
	em       *emit.Emitter
	resource []attribute.KeyValue
}

func run(ctx context.Context, f flags, log *slog.Logger) error {
	var dsn *ccfg.IngestDSN
	if f.dsn != "" {
		d, err := ccfg.ParseIngestDSN(f.dsn)
		if err != nil {
			return err
		}
		dsn = &d
	}
	newEmitter := func(interval time.Duration, res []attribute.KeyValue) (*emit.Emitter, error) {
		if dsn == nil {
			return nil, nil
		}
		em, err := emit.New(ctx, emit.Options{MetricsURL: dsn.MetricsURL, Key: dsn.Key, Interval: interval, Version: version, Logger: log, Resource: res})
		if err != nil {
			return nil, err
		}
		return em, nil
	}
	shutdown := func(name string, fn func(context.Context) error) {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := fn(sctx); err != nil {
			log.Warn("flush on shutdown failed", "emitter", name, "err", err)
		}
	}

	// host / cluster modes (contracts §20)
	var groups []group
	var events *eventSink
	if f.has(infra.ModeHost) || f.has(infra.ModeCluster) {
		ic := infra.ConfigFromEnv()
		if f.has(infra.ModeHost) {
			hostName := ic.FS.HostName()
			res := infra.HostResource(ctx, ic, hostName)
			em, err := newEmitter(infra.HostInterval, res)
			if err != nil {
				return err
			}
			groups = append(groups, group{name: "host", targets: infra.HostTargets(ic, hostName, log), em: em, resource: res})
		}
		if f.has(infra.ModeCluster) {
			res := infra.ClusterResource(ctx, ic)
			em, err := newEmitter(infra.ClusterInterval, res)
			if err != nil {
				return err
			}
			events = &eventSink{print: f.once}
			if dsn != nil {
				logRes := []attribute.KeyValue{attribute.String("service.name", "k8s"), attribute.String("service.version", version)}
				if ic.Cluster != "" {
					logRes = append(logRes, attribute.String(emit.ResK8sCluster, ic.Cluster))
				}
				le, err := emit.NewLogEmitter(ctx, emit.LogOptions{LogsURL: dsn.LogsURL, Key: dsn.Key, Resource: logRes})
				if err != nil {
					return err
				}
				events.next = le
				defer shutdown("k8s-events", le.Shutdown)
			}
			groups = append(groups, group{name: "cluster", targets: []agent.Target{infra.ClusterTarget(ic, events)}, em: em, resource: res})
		}
		for _, g := range groups {
			if g.em != nil {
				defer shutdown(g.name, g.em.Shutdown)
			}
		}
	}

	// db mode (contracts §5): targets from static YAML and/or the api
	var static *ccfg.Config
	var load func(ctx context.Context) (*ccfg.Config, error)
	var cfg *ccfg.Config
	opts := agent.Options{Host: scrape.HostOptions{Enabled: f.hostMetrics, DiskPath: f.hostDiskPath}}
	var dbEm *emit.Emitter
	if f.has(infra.ModeDB) {
		if f.configPath != "" {
			c, err := ccfg.LoadFile(f.configPath)
			if err != nil {
				return err
			}
			static = c
		}
		fetcher := ccfg.Fetcher{APIURL: f.apiURL, APIKey: f.apiKey}
		load = func(ctx context.Context) (*ccfg.Config, error) {
			if f.apiURL == "" {
				return ccfg.Merge(static, nil), nil
			}
			remote, err := fetcher.Fetch(ctx)
			if err != nil {
				return nil, err
			}
			return ccfg.Merge(static, remote), nil
		}
		for _, m := range strings.Split(f.modes, ",") {
			if m = strings.TrimSpace(m); m != "" {
				opts.Modes = append(opts.Modes, m)
			}
		}
		var err error
		cfg, err = load(ctx)
		if err != nil {
			if static == nil {
				log.Warn("remote config unavailable, waiting for the next poll", "err", err)
			} else {
				log.Warn("remote config unavailable, using static config only", "err", err)
			}
			cfg = ccfg.Merge(static, nil)
		}
		if dbEm, err = newEmitter(cfg.Interval.D(), nil); err != nil {
			return err
		}
		if dbEm != nil {
			defer shutdown("db", dbEm.Shutdown)
		}
	}

	if f.once {
		all := groups
		if cfg != nil {
			all = append([]group{{name: "db", targets: agent.Targets(cfg, opts, log), em: dbEm}}, all...)
		}
		return runOnce(ctx, all, events, f.onceGap, log)
	}

	for _, g := range groups {
		a := agent.New(publisherOf(g.em), log)
		a.Apply(ctx, g.targets)
		log.Info("trooper-agent mode started", "mode", g.name, "targets", strings.Join(a.IDs(), ","), "version", version)
		defer a.Stop()
	}
	if cfg == nil {
		<-ctx.Done()
		return nil
	}

	a := agent.New(dbEm, log)
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

// publisherOf avoids a typed-nil *Emitter inside the Publisher interface.
func publisherOf(em *emit.Emitter) agent.Publisher {
	if em == nil {
		return &recorder{last: map[string]emit.Result{}}
	}
	return em
}

// eventSink forwards Warning events to the log emitter and, in --once mode, prints them.
type eventSink struct {
	next  *emit.LogEmitter
	print bool
	mu    sync.Mutex
	seen  []emit.LogRecord
}

func (s *eventSink) Warn(ctx context.Context, r emit.LogRecord) {
	if s.print {
		s.mu.Lock()
		s.seen = append(s.seen, r)
		s.mu.Unlock()
	}
	if s.next != nil {
		s.next.Warn(ctx, r)
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

func attrMap(kvs []attribute.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

func runOnce(ctx context.Context, groups []group, events *eventSink, gap time.Duration, log *slog.Logger) error {
	type running struct {
		g       group
		rec     *recorder
		runners []*agent.Runner
	}
	var all []running
	total := 0
	for _, g := range groups {
		rn := running{g: g, rec: &recorder{next: g.em, last: map[string]emit.Result{}}}
		for _, t := range g.targets {
			r := &agent.Runner{T: t, Pub: rn.rec, Log: log}
			defer r.Close()
			r.Once(ctx)
			rn.runners = append(rn.runners, r)
			total++
		}
		all = append(all, rn)
	}
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(gap):
	}
	failed := 0
	for _, rn := range all {
		for _, r := range rn.runners {
			if !r.Once(ctx) {
				failed++
			}
		}
	}
	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, rn := range all {
		if rn.g.em != nil {
			if err := rn.g.em.Flush(fctx); err != nil {
				return fmt.Errorf("flush %s: %w", rn.g.name, err)
			}
		}
	}
	if events != nil && events.next != nil {
		if err := events.next.Flush(fctx); err != nil {
			return fmt.Errorf("flush events: %w", err)
		}
	}
	enc := json.NewEncoder(os.Stdout)
	for _, rn := range all {
		if len(rn.g.resource) > 0 {
			_ = enc.Encode(map[string]any{"mode": rn.g.name, "type": "resource", "attributes": attrMap(rn.g.resource)})
		}
		for _, t := range rn.g.targets {
			res := rn.rec.last[t.ID]
			for _, kind := range []struct {
				typ string
				s   []emit.Sample
			}{{"gauge", res.Gauges}, {"delta_sum", res.Counts}} {
				for _, s := range kind.s {
					_ = enc.Encode(map[string]any{"target": t.ID, "type": kind.typ, "metric": s.Name, "value": s.Value, "attributes": attrMap(s.Attrs.ToSlice())})
				}
			}
		}
	}
	if events != nil {
		for _, e := range events.seen {
			_ = enc.Encode(map[string]any{"type": "log", "service": "k8s", "severity": "warn", "ts": e.Time, "body": e.Body, "attributes": attrMap(e.Attrs)})
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d targets failed", failed, total)
	}
	return nil
}
