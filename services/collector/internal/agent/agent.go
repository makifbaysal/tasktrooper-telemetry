// Package agent runs one scrape loop per target, reconciles the target list when the config
// changes and publishes results to the emitter. A failing target never stops the agent: errors
// are logged (rate-limited) and reported as trooper.collector.up = 0.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry/services/collector/internal/config"
	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/scrape"
)

// Publisher receives scrape results (the emitter).
type Publisher interface {
	Publish(ctx context.Context, target string, r emit.Result)
	Remove(target string)
}

// Target is one thing to scrape.
type Target struct {
	// ID is unique across kinds ("db/orders-db", "mq/kafka-main").
	ID       string
	Key      string // trooper.db.id / trooper.broker.cluster
	Kind     string // postgres|mysql|redis|mongodb|kafka|rabbitmq
	Interval time.Duration
	Timeout  time.Duration
	// Spec hash: a changed spec restarts the target's loop.
	Spec string
	New  func() (scrape.Scraper, error)
}

// Options configure target selection.
type Options struct {
	// Modes: which `collector` values this agent scrapes (agent, direct, cloud). Empty = all.
	Modes []string
	Host  scrape.HostOptions
}

// Targets turns a config into targets.
func Targets(c *config.Config, o Options, log *slog.Logger) []Target {
	var out []Target
	want := func(mode string) bool { return len(o.Modes) == 0 || slices.Contains(o.Modes, mode) }
	for _, d := range c.Databases {
		if !want(d.Collector) {
			continue
		}
		d := d
		out = append(out, Target{
			ID: "db/" + d.Key(), Key: d.Key(), Kind: d.Engine,
			Interval: d.Interval.D(), Timeout: d.Timeout.D(),
			Spec: specHash(d, o.Host),
			New:  func() (scrape.Scraper, error) { return scrape.NewDatabase(d, o.Host, log) },
		})
	}
	for _, b := range c.BrokerClusters {
		if !want(b.Collector) {
			continue
		}
		b := b
		out = append(out, Target{
			ID: "mq/" + b.Key(), Key: b.Key(), Kind: b.Kind,
			Interval: b.Interval.D(), Timeout: b.Timeout.D(),
			Spec: specHash(b, nil),
			New:  func() (scrape.Scraper, error) { return scrape.NewBroker(b, log) },
		})
	}
	return out
}

func specHash(v ...any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// Agent owns the running loops.
type Agent struct {
	pub Publisher
	log *slog.Logger

	mu    sync.Mutex
	loops map[string]*loop
	wg    sync.WaitGroup
}

type loop struct {
	t      Target
	cancel context.CancelFunc
	done   chan struct{}
}

// New returns an agent publishing to pub.
func New(pub Publisher, log *slog.Logger) *Agent {
	return &Agent{pub: pub, log: log, loops: map[string]*loop{}}
}

// Apply reconciles running loops with targets: starts new ones, restarts changed ones, stops
// removed ones.
func (a *Agent) Apply(ctx context.Context, targets []Target) {
	a.mu.Lock()
	defer a.mu.Unlock()
	keep := map[string]bool{}
	for _, t := range targets {
		keep[t.ID] = true
		if l, ok := a.loops[t.ID]; ok {
			if l.t.Spec == t.Spec {
				continue
			}
			a.log.Info("target changed, restarting", "target", t.ID)
			a.stopLocked(t.ID)
		}
		a.startLocked(ctx, t)
	}
	for id := range a.loops {
		if !keep[id] {
			a.log.Info("target removed", "target", id)
			a.stopLocked(id)
			a.pub.Remove(id)
		}
	}
}

// IDs returns the running target ids (sorted).
func (a *Agent) IDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]string, 0, len(a.loops))
	for id := range a.loops {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (a *Agent) startLocked(ctx context.Context, t Target) {
	lctx, cancel := context.WithCancel(ctx)
	l := &loop{t: t, cancel: cancel, done: make(chan struct{})}
	a.loops[t.ID] = l
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer close(l.done)
		a.run(lctx, t)
	}()
	a.log.Info("target started", "target", t.ID, "kind", t.Kind, "interval", t.Interval.String())
}

func (a *Agent) stopLocked(id string) {
	if l, ok := a.loops[id]; ok {
		l.cancel()
		<-l.done
		delete(a.loops, id)
	}
}

// Stop stops every loop.
func (a *Agent) Stop() {
	a.mu.Lock()
	for id := range a.loops {
		a.loops[id].cancel()
	}
	a.mu.Unlock()
	a.wg.Wait()
	a.mu.Lock()
	a.loops = map[string]*loop{}
	a.mu.Unlock()
}

// Runner scrapes one target; it owns the scraper and recreates it after errors in New.
type Runner struct {
	T       Target
	Pub     Publisher
	Log     *slog.Logger
	s       scrape.Scraper
	lastErr string
	errAt   time.Time
}

// Once performs one scrape (creating the scraper if needed) and publishes the result.
// It never panics and never returns an error: failures become trooper.collector.up = 0.
func (r *Runner) Once(ctx context.Context) (ok bool) {
	up := emit.UpAttrs(r.T.Key, r.T.Kind)
	defer func() {
		if p := recover(); p != nil {
			r.fail(ctx, up, fmt.Errorf("panic: %v\n%s", p, debug.Stack()))
			ok = false
			if r.s != nil {
				_ = r.s.Close()
				r.s = nil
			}
		}
	}()
	if r.s == nil {
		s, err := r.T.New()
		if err != nil {
			r.fail(ctx, up, err)
			return false
		}
		r.s = s
	}
	sctx, cancel := context.WithTimeout(ctx, r.T.Timeout)
	defer cancel()
	res, err := r.s.Scrape(sctx)
	if err != nil {
		r.fail(ctx, up, err)
		return false
	}
	if r.lastErr != "" {
		r.Log.Info("target recovered", "target", r.T.ID)
		r.lastErr = ""
	}
	res.Gauge(emit.CollectorUp, 1, up...)
	r.Pub.Publish(ctx, r.T.ID, res)
	return true
}

func (r *Runner) fail(ctx context.Context, up []attribute.KeyValue, err error) {
	msg := err.Error()
	// log every distinct error, repeats at most every 5 minutes
	if msg != r.lastErr || time.Since(r.errAt) > 5*time.Minute {
		r.Log.Warn("scrape failed", "target", r.T.ID, "kind", r.T.Kind, "err", firstLine(msg))
		r.lastErr, r.errAt = msg, time.Now()
	}
	var res emit.Result
	res.Gauge(emit.CollectorUp, 0, up...)
	r.Pub.Publish(ctx, r.T.ID, res)
}

// Close closes the scraper.
func (r *Runner) Close() {
	if r.s != nil {
		_ = r.s.Close()
		r.s = nil
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func (a *Agent) run(ctx context.Context, t Target) {
	r := &Runner{T: t, Pub: a.pub, Log: a.log}
	defer r.Close()
	// spread targets over the interval so they do not all hit at once
	jitter := time.Duration(rand.Int64N(int64(t.Interval)/4 + 1))
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}
	tk := time.NewTicker(t.Interval)
	defer tk.Stop()
	for {
		r.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
	}
}
