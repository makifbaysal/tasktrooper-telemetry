package scrape

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry/services/collector/internal/config"
	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/rate"
)

// Redis scrapes a Redis / Valkey server through INFO.
//
//   - connections: connected_clients / maxclients (INFO, fallback CONFIG GET maxclients)
//   - ops: Δtotal_commands_processed / s (instantaneous_ops_per_sec on the first scrape)
//   - memory: used_memory / maxmemory (used_memory / total_system_memory when maxmemory = 0)
//   - hit_ratio: Δkeyspace_hits / Δ(hits + misses); evictions: Δevicted_keys / min
//   - latency.p95: p95 of PING round-trips sampled every 500 ms in the background
//   - slow: new SLOWLOG entries / min
//   - replication_lag (ms): on a master one series per replica from slaveN lag= (seconds since
//     last ACK); on a replica master_last_io_seconds_ago (or link-down duration)
//   - top commands: INFO commandstats deltas (calls, usec → total_ms); no rows / p95
type Redis struct {
	t   config.Database
	cl  *redis.Client
	log *slog.Logger
	c   *rate.Counters

	mu       sync.Mutex
	pings    []float64
	stop     chan struct{}
	wg       sync.WaitGroup
	slowID   int64
	slowInit bool
	slowAt   time.Time
	lastCmds time.Time
}

// NewRedis connects and starts the background PING sampler.
func NewRedis(t config.Database, log *slog.Logger) (*Redis, error) {
	dsn, err := t.ConnString()
	if err != nil {
		return nil, err
	}
	opt, err := redis.ParseURL(dsn)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	opt.PoolSize = 2
	opt.MinIdleConns = 1
	opt.DialTimeout = t.Timeout.D()
	opt.ReadTimeout = t.Timeout.D()
	opt.ClientName = "trooper-agent"
	r := &Redis{t: t, cl: redis.NewClient(opt), log: log, c: rate.NewCounters(), stop: make(chan struct{})}
	r.wg.Add(1)
	go r.pinger(500 * time.Millisecond)
	return r, nil
}

func (r *Redis) pinger(every time.Duration) {
	defer r.wg.Done()
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-tk.C:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			start := time.Now()
			err := r.cl.Ping(ctx).Err()
			cancel()
			if err != nil {
				continue
			}
			ms := float64(time.Since(start).Microseconds()) / 1000
			r.mu.Lock()
			if len(r.pings) < 10_000 {
				r.pings = append(r.pings, ms)
			}
			r.mu.Unlock()
		}
	}
}

// Close stops the sampler and the client.
func (r *Redis) Close() error {
	close(r.stop)
	r.wg.Wait()
	return r.cl.Close()
}

// ParseInfo parses INFO output into section-less key → value.
func ParseInfo(s string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[k] = v
		}
	}
	return out
}

// ParseKV parses "a=1,b=2" (slaveN, cmdstat_*, dbN lines).
func ParseKV(s string) map[string]string {
	out := map[string]string{}
	for part := range strings.SplitSeq(s, ",") {
		if k, v, ok := strings.Cut(part, "="); ok {
			out[k] = v
		}
	}
	return out
}

func num(m map[string]string, k string) (float64, bool) {
	v, ok := m[k]
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	return f, err == nil
}

// Scrape implements Scraper.
func (r *Redis) Scrape(ctx context.Context) (emit.Result, error) {
	var res emit.Result
	now := time.Now()
	raw, err := r.cl.Info(ctx, "all").Result()
	if err != nil {
		return res, fmt.Errorf("INFO: %w", err)
	}
	info := ParseInfo(raw)
	role := "primary"
	if info["role"] == "slave" || info["role"] == "replica" {
		role = "replica"
	}
	base := emit.DBAttrs(r.t.Key(), r.t.DisplayName(), "redis", r.t.NodeName(), role)

	if v, ok := num(info, "connected_clients"); ok {
		res.Gauge(emit.DBConnections, v, base...)
	}
	if v, ok := num(info, "maxclients"); ok {
		res.Gauge(emit.DBMaxConnections, v, base...)
	} else if cfg, err := r.cl.ConfigGet(ctx, "maxclients").Result(); err == nil {
		if v, err := strconv.ParseFloat(cfg["maxclients"], 64); err == nil {
			res.Gauge(emit.DBMaxConnections, v, base...)
		}
	}
	if total, ok := num(info, "total_commands_processed"); ok {
		if ops, ok := r.c.Rate("cmds", total, now); ok {
			res.Gauge(emit.DBOps, ops, base...)
		} else if v, ok := num(info, "instantaneous_ops_per_sec"); ok {
			res.Gauge(emit.DBOps, v, base...)
		}
	}
	used, _ := num(info, "used_memory")
	if maxm, _ := num(info, "maxmemory"); maxm > 0 {
		if f, ok := rate.Fraction(used, maxm); ok {
			res.Gauge(emit.DBMemory, f, base...)
		}
	} else if sys, _ := num(info, "total_system_memory"); sys > 0 {
		if f, ok := rate.Fraction(used, sys); ok {
			res.Gauge(emit.DBMemory, f, base...)
		}
	}
	hits, _ := num(info, "keyspace_hits")
	misses, _ := num(info, "keyspace_misses")
	dh, _, ok1 := r.c.Delta("hits", hits, now)
	dm, _, ok2 := r.c.Delta("misses", misses, now)
	if hr, ok := rate.IntervalHitRatio(dh, dm, ok1 && ok2, hits, misses); ok {
		res.Gauge(emit.DBHitRatio, hr, base...)
	}
	if ev, ok := num(info, "evicted_keys"); ok {
		if d, dt, ok := r.c.Delta("evicted", ev, now); ok {
			res.Gauge(emit.DBEvictions, rate.PerMinute(d, dt), base...)
		}
	}

	r.mu.Lock()
	pings := r.pings
	r.pings = nil
	r.mu.Unlock()
	if p95, ok := rate.Quantile(pings, 0.95); ok {
		res.Gauge(emit.DBLatencyP95, p95, base...)
	}

	r.replication(info, role, base, &res)
	r.slowlog(ctx, now, base, &res)
	r.commands(info, now, &res)
	return res, nil
}

func (r *Redis) replication(info map[string]string, role string, base []attribute.KeyValue, res *emit.Result) {
	if role == "replica" {
		if info["master_link_status"] == "down" {
			if s, ok := num(info, "master_link_down_since_seconds"); ok {
				res.Gauge(emit.DBReplLag, s*1000, base...)
			}
			return
		}
		if s, ok := num(info, "master_last_io_seconds_ago"); ok && s >= 0 {
			res.Gauge(emit.DBReplLag, s*1000, base...)
		}
		return
	}
	n, _ := num(info, "connected_slaves")
	for i := 0; i < int(n); i++ {
		kv := ParseKV(info["slave"+strconv.Itoa(i)])
		lag, ok := num(kv, "lag")
		if !ok {
			continue
		}
		node := kv["ip"] + ":" + kv["port"]
		a := emit.DBAttrs(r.t.Key(), r.t.DisplayName(), "redis", node, "replica")
		res.Gauge(emit.DBReplLag, lag*1000, a...)
	}
}

func (r *Redis) slowlog(ctx context.Context, now time.Time, base []attribute.KeyValue, res *emit.Result) {
	logs, err := r.cl.SlowLogGet(ctx, 128).Result()
	if err != nil {
		return // SLOWLOG is often disabled on managed Redis
	}
	newest := r.slowID
	var count float64
	for _, l := range logs {
		newest = max(newest, l.ID)
		if r.slowInit && l.ID > r.slowID {
			count++
		}
	}
	if r.slowInit {
		res.Gauge(emit.DBSlow, rate.PerMinute(count, now.Sub(r.slowAt)), base...)
	}
	r.slowInit, r.slowID, r.slowAt = true, newest, now
}

func (r *Redis) commands(info map[string]string, now time.Time, res *emit.Result) {
	type cmd struct {
		name        string
		dCalls, dMs float64
	}
	since := r.lastCmds
	r.lastCmds = now
	var cmds []cmd
	var rank []float64
	for k, v := range info {
		name, ok := strings.CutPrefix(k, "cmdstat_")
		if !ok {
			continue
		}
		kv := ParseKV(v)
		calls, ok1 := num(kv, "calls")
		usec, ok2 := num(kv, "usec")
		if !ok1 || !ok2 {
			continue
		}
		dc, ok := r.c.NewKeyDelta("cmd_calls/"+name, calls, now, since)
		du, _ := r.c.NewKeyDelta("cmd_usec/"+name, usec, now, since)
		if !ok || dc <= 0 {
			continue
		}
		cmds = append(cmds, cmd{name: strings.ToUpper(strings.ReplaceAll(name, "|", " ")), dCalls: dc, dMs: du / 1000})
		rank = append(rank, du/1000+1e-9*dc)
	}
	for _, i := range TopN(rank, r.t.TopQueries) {
		c := cmds[i]
		a := []attribute.KeyValue{
			attribute.String(emit.AttrDBID, r.t.Key()),
			attribute.String(emit.AttrQueryHash, HashText("redis:"+c.name)),
			attribute.String(emit.AttrQueryText, c.name),
		}
		res.Count(emit.DBQueryCalls, c.dCalls, a...)
		res.Count(emit.DBQueryTotalMs, c.dMs, a...)
	}
}
