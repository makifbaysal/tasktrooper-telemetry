// Package profsim uploads synthetic continuous profiles (contracts §24, ADR-022) for the demo services:
// per pod of the simulated EKS cluster (infrasim pod names as host), every minute a CPU (10 s window),
// alloc_space (60 s) and inuse_space profile in the folded format, with `# trooper.route=` sections so the
// endpoint flame graphs work. Two versions: the service's previous version is backfilled for the last
// --profile-backfill (the "before deploy" baseline) and the current version (world.Services) is live, so
// the version diff shows the stories in stacks.go — orders-api v2.41.0 compiles promo regexps per request
// (pricing.ApplyDiscounts ↑), catalog-api v3.2.0 added semantic search, recommendation-api v0.14.0 got
// faster sorting.
package profsim

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tasktrooper/telemetry/tools/demo/internal/infrasim"
	"github.com/tasktrooper/telemetry/tools/demo/internal/world"
)

// Types uploaded per pod and period.
var Types = []struct{ Type, Unit string }{
	{"cpu", "nanoseconds"},
	{"alloc_space", "bytes"},
	{"inuse_space", "bytes"},
}

const (
	cpuWindow  = 10 * time.Second
	period     = time.Minute
	bgShare    = 0.15 // CPU / allocations not attributed to a route
	mib        = 1 << 20
	maxRetries = 3
)

// Sim uploads the profiles.
type Sim struct {
	IngestURL, Key string
	Log            *slog.Logger
	Backfill       time.Duration // history of the previous version uploaded at start (0 = none)

	client     *http.Client
	Sent, Errs atomic.Int64
}

// New returns a simulator for the ingest endpoint (http://host:port) and key.
func New(ingestURL, key string, backfill time.Duration, log *slog.Logger) *Sim {
	return &Sim{IngestURL: strings.TrimSuffix(ingestURL, "/"), Key: key, Log: log, Backfill: backfill,
		client: &http.Client{Timeout: 15 * time.Second}}
}

type target struct {
	spec    serviceSpec
	version string
	hosts   []string
}

func targets() []target {
	var out []target
	for _, s := range specs() {
		def, ok := world.ServiceByName(s.name)
		if !ok {
			continue
		}
		hosts := infrasim.PodNames(s.name)
		if len(hosts) == 0 {
			hosts = []string{s.name + "-0"}
		}
		out = append(out, target{spec: s, version: def.Version, hosts: hosts})
	}
	return out
}

// Run backfills, then uploads every minute until ctx ends.
func (s *Sim) Run(ctx context.Context) {
	ts := targets()
	if s.Backfill > 0 {
		s.backfill(ctx, ts)
	}
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		now := time.Now()
		var wg sync.WaitGroup
		for _, tg := range ts {
			for _, host := range tg.hosts {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.uploadAll(ctx, tg.spec, tg.version, false, host, now)
				}()
			}
		}
		wg.Wait()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Sim) backfill(ctx context.Context, ts []target) {
	n := int(s.Backfill / period)
	now := time.Now().Truncate(period)
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i := n; i >= 1; i-- {
		at := now.Add(-time.Duration(i) * period)
		for _, tg := range ts {
			for _, host := range tg.hosts {
				if ctx.Err() != nil {
					wg.Wait()
					return
				}
				sem <- struct{}{}
				wg.Add(1)
				go func() {
					defer func() { <-sem; wg.Done() }()
					s.uploadAll(ctx, tg.spec, tg.spec.prevVersion, true, host, at)
				}()
			}
		}
	}
	wg.Wait()
	s.Log.Info("profiles backfilled", "minutes", n, "versions", "previous", "uploads", s.Sent.Load(), "errors", s.Errs.Load())
}

func (s *Sim) uploadAll(ctx context.Context, spec serviceSpec, version string, prev bool, host string, end time.Time) {
	rng := rand.New(rand.NewPCG(uint64(end.UnixNano()), fnv(host)))
	f := factor(end)
	for _, ty := range Types {
		var start time.Time
		switch ty.Type {
		case "cpu":
			start = end.Add(-cpuWindow)
		default:
			start = end.Add(-period)
		}
		body := Folded(spec, ty.Type, prev, f, len(hostsOf(spec)), rng)
		if err := s.upload(ctx, spec.name, version, host, ty.Type, ty.Unit, start, end, body); err != nil {
			s.Errs.Add(1)
			if s.Errs.Load()%20 == 1 {
				s.Log.Warn("profile upload failed", "service", spec.name, "type", ty.Type, "err", err)
			}
			continue
		}
		s.Sent.Add(1)
	}
}

var hostCache sync.Map

func hostsOf(spec serviceSpec) []string {
	if v, ok := hostCache.Load(spec.name); ok {
		return v.([]string)
	}
	h := infrasim.PodNames(spec.name)
	if len(h) == 0 {
		h = []string{spec.name}
	}
	hostCache.Store(spec.name, h)
	return h
}

func (s *Sim) upload(ctx context.Context, service, version, host, typ, unit string, start, end time.Time, body []byte) error {
	q := url.Values{
		"service": {service}, "env": {world.Environment}, "version": {version}, "host": {host}, "runtime": {"go"},
		"start": {strconv.FormatInt(start.UnixMilli(), 10)}, "end": {strconv.FormatInt(end.UnixMilli(), 10)},
		"format": {"folded"}, "type": {typ}, "unit": {unit},
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(body)
	_ = zw.Close()
	backoff := time.Second
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.IngestURL+"/v1/profiles?"+q.Encode(), bytes.NewReader(buf.Bytes()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Content-Encoding", "gzip")
		req.Header.Set("x-trooper-key", s.Key)
		resp, err := s.client.Do(req)
		if err != nil {
			return err
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return nil
		}
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < maxRetries {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
			continue
		}
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
}

// factor is the daily traffic curve (same shape as sim.Traffic, peak ~15:00).
func factor(t time.Time) float64 {
	h := float64(t.Hour()) + float64(t.Minute())/60
	return 1 + 0.25*math.Sin((h-9)/24*2*math.Pi)
}

func fnv(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// expand replaces building-block keys in a ";"-separated path by their frames.
func expand(path string) string {
	parts := strings.Split(path, ";")
	for i, p := range parts {
		if b, ok := blocks[p]; ok {
			parts[i] = b
		}
	}
	return strings.Join(parts, ";")
}

func active(o op, prev bool) (mult float64, ok bool) {
	if prev {
		return 1, !o.onlyNew
	}
	if o.onlyOld {
		return 0, false
	}
	if o.newX > 0 {
		return o.newX, true
	}
	return 1, true
}

// Folded renders one profile of a pod: typ cpu | alloc_space | inuse_space; prev = previous version;
// f = traffic factor; replicas = pods of the service (the per-service rate is split across them).
func Folded(spec serviceSpec, typ string, prev bool, f float64, replicas int, rng *rand.Rand) []byte {
	replicas = max(replicas, 1)
	noise := func() float64 { return math.Max(0.2, 1+rng.NormFloat64()*0.12) }
	var b strings.Builder
	line := func(stack string, v float64) {
		if v >= 1 {
			fmt.Fprintf(&b, "%s %d\n", stack, int64(v))
		}
	}
	if typ == "inuse_space" {
		hs := append(append([]heap(nil), heapCommon...), spec.heap...)
		for _, x := range hs {
			if x.onlyNew && prev {
				continue
			}
			m := 1.0
			if x.newX > 0 && !prev {
				m = x.newX
			}
			line(expand(x.path), x.mib*mib*m*math.Max(0.5, 1+rng.NormFloat64()*0.05))
		}
		return []byte(b.String())
	}
	// total of the pod over the window (old-version weights define the baseline; a regression adds to it)
	var total float64
	weight := func(o op) float64 {
		if typ == "cpu" {
			return o.cpu
		}
		return o.alloc
	}
	if typ == "cpu" {
		total = spec.cores / float64(replicas) * f * float64(cpuWindow.Nanoseconds())
	} else {
		total = spec.allocMiB / float64(replicas) * f * mib
	}
	routes := append([]route(nil), spec.routes...)
	sort.SliceStable(routes, func(i, j int) bool { return routes[i].share > routes[j].share })
	for _, r := range routes {
		base := 0.0
		for _, o := range r.ops {
			if !o.onlyNew {
				base += weight(o)
			}
		}
		if base == 0 {
			continue
		}
		fmt.Fprintf(&b, "# trooper.route=%s trooper.method=%s\n", r.route, r.method)
		for _, o := range r.ops {
			m, ok := active(o, prev)
			if !ok {
				continue
			}
			v := total * (1 - bgShare) * r.share * weight(o) * m / base * noise()
			line(r.handler+";"+expand(o.path), v)
		}
	}
	b.WriteString("#\n")
	base := 0.0
	for _, o := range background {
		base += weight(o)
	}
	for _, o := range background {
		line(expand(o.path), total*bgShare*weight(o)/base*noise())
	}
	return []byte(b.String())
}
