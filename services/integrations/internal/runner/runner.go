// Package runner schedules discovery and metric collection per integration. Every integration runs in
// its own goroutine with its own timeouts and panic recovery, so one broken integration never affects others.
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
	"github.com/tasktrooper/telemetry/services/integrations/internal/otlp"
)

// Store is the subset of store.Store the runner needs.
type Store interface {
	Integrations(ctx context.Context) ([]model.Integration, error)
	SetStatus(ctx context.Context, id int64, status, errMsg string, resources int) error
	SyncDatabases(ctx context.Context, in model.Integration, dbs []model.Database) ([]string, error)
	SyncBrokers(ctx context.Context, in model.Integration, brokers []model.Broker) ([]string, error)
	PatchDatabaseMeta(ctx context.Context, in model.Integration, patches map[string]map[string]any) error
	UpsertDeploys(ctx context.Context, in model.Integration, ds []model.DeployMarker) error
}

// Sender exports samples.
type Sender interface {
	Send(ctx context.Context, t otlp.Target, samples []model.Sample) error
}

// Factory builds a provider for an integration (resolving its credentials).
type Factory func(ctx context.Context, in model.Integration) (model.Provider, error)

// Options configure the runner.
type Options struct {
	Store    Store
	Sender   Sender
	Factory  Factory
	Log      *slog.Logger
	Reload   time.Duration // integrations reload (30s)
	Discover time.Duration // discovery interval (10m)
	Metrics  time.Duration // metrics interval (60s)
	// DiscoverTimeout / MetricsTimeout bound one step.
	DiscoverTimeout time.Duration
	MetricsTimeout  time.Duration
	// RetryDelay after a failed provider build or discovery (default 1m).
	RetryDelay time.Duration
}

// Runner owns the integration workers.
type Runner struct {
	o       Options
	mu      sync.Mutex
	workers map[int64]*worker
}

type worker struct {
	hash      [32]byte
	cancel    context.CancelFunc
	done      chan struct{}
	trigger   chan struct{}
	requested time.Time // last syncRequestedAt served
}

// SplitSyncRequest returns config without `syncRequestedAt` (so a sync request does not restart the
// worker) and the requested time. The api writes epoch ms (number or string) or an ISO-8601 string.
func SplitSyncRequest(cfg json.RawMessage) (json.RawMessage, time.Time) {
	var m map[string]json.RawMessage
	if len(cfg) == 0 || json.Unmarshal(cfg, &m) != nil {
		return cfg, time.Time{}
	}
	raw, ok := m["syncRequestedAt"]
	if !ok {
		return cfg, time.Time{}
	}
	delete(m, "syncRequestedAt")
	rest, _ := json.Marshal(m) // map keys are sorted → stable hash
	var t time.Time
	var n float64
	var s string
	switch {
	case json.Unmarshal(raw, &n) == nil && n > 0:
		t = time.UnixMilli(int64(n))
	case json.Unmarshal(raw, &s) == nil && s != "":
		if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
			t = time.UnixMilli(ms)
		} else if p, err := time.Parse(time.RFC3339Nano, s); err == nil {
			t = p
		}
	}
	return rest, t
}

// New returns a runner with defaults filled in.
func New(o Options) *Runner {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.Reload, 30*time.Second)
	def(&o.Discover, 10*time.Minute)
	def(&o.Metrics, 60*time.Second)
	def(&o.DiscoverTimeout, 5*time.Minute)
	def(&o.MetricsTimeout, 50*time.Second)
	def(&o.RetryDelay, time.Minute)
	return &Runner{o: o, workers: map[int64]*worker{}}
}

// Run reloads integrations until ctx is done, then stops every worker.
func (r *Runner) Run(ctx context.Context) error {
	t := time.NewTicker(r.o.Reload)
	defer t.Stop()
	for {
		if err := r.Reconcile(ctx); err != nil {
			r.o.Log.Error("load integrations", "err", err)
		}
		select {
		case <-ctx.Done():
			r.StopAll()
			return nil
		case <-t.C:
		}
	}
}

// Reconcile starts workers for new/changed integrations and stops removed/disabled ones.
func (r *Runner) Reconcile(ctx context.Context) error {
	list, err := r.o.Store.Integrations(ctx)
	if err != nil {
		return err
	}
	seen := map[int64]bool{}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, in := range list {
		seen[in.ID] = true
		cfg, requested := SplitSyncRequest(in.Config)
		h := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%s|%s", in.ProjectID, in.Provider, in.Name, cfg, in.CredentialsRef)))
		if w, ok := r.workers[in.ID]; ok {
			if w.hash == h {
				// POST /integrations/{id}/sync: syncRequestedAt newer than last_sync_at → sync now (once per request).
				if !requested.IsZero() && !requested.Equal(w.requested) && (in.LastSyncAt == nil || requested.After(*in.LastSyncAt)) {
					w.requested = requested
					select {
					case w.trigger <- struct{}{}:
					default:
					}
				}
				continue
			}
			w.cancel()
			<-w.done
		}
		wctx, cancel := context.WithCancel(ctx)
		// A new worker syncs immediately, which also serves a pending request.
		w := &worker{hash: h, cancel: cancel, done: make(chan struct{}), trigger: make(chan struct{}, 1), requested: requested}
		r.workers[in.ID] = w
		go func(in model.Integration) {
			defer close(w.done)
			r.work(wctx, in, w.trigger)
		}(in)
	}
	for id, w := range r.workers {
		if !seen[id] {
			w.cancel()
			<-w.done
			delete(r.workers, id)
		}
	}
	return nil
}

// StopAll cancels every worker and waits.
func (r *Runner) StopAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, w := range r.workers {
		w.cancel()
		<-w.done
		delete(r.workers, id)
	}
}

// RunOnce builds the provider, discovers and collects once (tests, `--once`).
func (r *Runner) RunOnce(ctx context.Context, in model.Integration) error {
	s := &session{r: r, in: in, log: r.o.Log.With("integration", in.ID, "provider", in.Provider)}
	if err := s.build(ctx); err != nil {
		return err
	}
	if err := s.discover(ctx); err != nil && s.inv == nil {
		return err
	}
	return s.collect(ctx)
}

type session struct {
	r     *Runner
	in    model.Integration
	log   *slog.Logger
	p     model.Provider
	inv   *model.Inventory
	owned map[string]bool
	// discoverErr is kept so a successful metrics step doesn't hide a discovery error.
	discoverErr error
	trigger     <-chan struct{}
	forced      bool // a sync request arrived: discover now
}

func (r *Runner) work(ctx context.Context, in model.Integration, trigger <-chan struct{}) {
	s := &session{r: r, in: in, log: r.o.Log.With("integration", in.ID, "provider", in.Provider), trigger: trigger}
	s.log.Info("integration started")
	defer s.log.Info("integration stopped")
	for {
		err := s.build(ctx)
		if err == nil {
			break
		}
		if !s.wait(ctx, r.o.RetryDelay) {
			return
		}
	}
	nextDiscover := time.Time{}
	for {
		now := time.Now()
		if !now.Before(nextDiscover) || s.inv == nil || s.forced {
			if s.forced {
				s.log.Info("sync requested")
			}
			s.forced = false
			err := s.discover(ctx)
			if ctx.Err() != nil {
				return
			}
			if s.inv == nil {
				nextDiscover = time.Now().Add(r.o.RetryDelay)
				_ = err
			} else {
				nextDiscover = time.Now().Add(r.o.Discover)
			}
		}
		if s.inv != nil {
			_ = s.collect(ctx)
		}
		if !s.wait(ctx, r.o.Metrics) {
			return
		}
	}
}

func (s *session) wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	case <-s.trigger: // nil channel (RunOnce) never fires
		s.forced = true
		return true
	}
}

func (s *session) build(ctx context.Context) (err error) {
	defer s.recover(&err)
	p, err := s.r.o.Factory(ctx, s.in)
	if err != nil {
		s.status(ctx, err, -1)
		return err
	}
	s.p = p
	return nil
}

func (s *session) discover(ctx context.Context) (err error) {
	defer s.recover(&err)
	cctx, cancel := context.WithTimeout(ctx, s.r.o.DiscoverTimeout)
	defer cancel()
	start := time.Now()
	inv, derr := s.p.Discover(cctx)
	if inv == nil {
		s.discoverErr = derr
		s.status(ctx, fmt.Errorf("discovery: %w", derr), -1)
		return derr
	}
	owned := map[string]bool{}
	dbSlugs, err := s.r.o.Store.SyncDatabases(cctx, s.in, inv.Databases)
	if err != nil {
		derr = errors.Join(derr, fmt.Errorf("store databases: %w", err))
	}
	brSlugs, err := s.r.o.Store.SyncBrokers(cctx, s.in, inv.Brokers)
	if err != nil {
		derr = errors.Join(derr, fmt.Errorf("store brokers: %w", err))
	}
	for _, sl := range append(dbSlugs, brSlugs...) {
		owned[sl] = true
	}
	s.inv, s.owned, s.discoverErr = inv, owned, derr
	if derr != nil {
		derr = fmt.Errorf("discovery: %w", derr)
	}
	s.status(ctx, derr, inv.Count())
	s.log.Info("discovery done", "databases", len(inv.Databases), "brokers", len(inv.Brokers), "other", inv.Other,
		"took", time.Since(start).Round(time.Millisecond), "err", errString(derr))
	return derr
}

func (s *session) collect(ctx context.Context) (err error) {
	defer s.recover(&err)
	cctx, cancel := context.WithTimeout(ctx, s.r.o.MetricsTimeout)
	defer cancel()
	res, cerr := s.p.Collect(cctx, s.inv, time.Now())
	if res != nil {
		samples := s.filter(res.Samples)
		if err := s.r.o.Sender.Send(cctx, otlp.Target{ProjectID: s.in.ProjectID, IntegrationID: s.in.ID, Provider: s.in.Provider, Name: s.in.Name}, samples); err != nil {
			cerr = errors.Join(cerr, err)
		}
		if err := s.r.o.Store.UpsertDeploys(cctx, s.in, res.Deploys); err != nil {
			cerr = errors.Join(cerr, fmt.Errorf("store deploys: %w", err))
		}
		if len(res.DBMeta) > 0 {
			if err := s.r.o.Store.PatchDatabaseMeta(cctx, s.in, res.DBMeta); err != nil {
				cerr = errors.Join(cerr, fmt.Errorf("store meta: %w", err))
			}
		}
		s.log.Debug("metrics sent", "samples", len(samples), "deploys", len(res.Deploys))
	}
	if cerr != nil {
		cerr = fmt.Errorf("metrics: %w", cerr)
		s.log.Warn("metrics step failed", "err", cerr.Error())
	}
	if s.discoverErr != nil {
		cerr = errors.Join(fmt.Errorf("discovery: %w", s.discoverErr), cerr)
	}
	s.status(ctx, cerr, s.inv.Count())
	return cerr
}

// filter drops samples of resources this integration does not own (slug collision with a manual row).
func (s *session) filter(in []model.Sample) []model.Sample {
	out := in[:0]
	for _, x := range in {
		if id, ok := x.Attrs["trooper.db.id"]; ok && !s.owned[id] {
			continue
		}
		if id, ok := x.Attrs["trooper.broker.cluster"]; ok && !s.owned[id] {
			continue
		}
		out = append(out, x)
	}
	return out
}

func (s *session) status(ctx context.Context, err error, resources int) {
	st, msg := "ok", ""
	if err != nil {
		st, msg = "error", strings.TrimSpace(err.Error())
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if e := s.r.o.Store.SetStatus(sctx, s.in.ID, st, msg, resources); e != nil {
		s.log.Error("update integration status", "err", e)
	}
}

func (s *session) recover(err *error) {
	if v := recover(); v != nil {
		s.log.Error("panic in integration", "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
		*err = fmt.Errorf("internal error: %v", v)
		s.status(context.Background(), *err, -1)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
