package infra

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/rate"
	"github.com/tasktrooper/telemetry/services/collector/internal/scrape"
)

// ---------------------------------------------------------------- source chain (host mode outside K8s)

// ContainerSource is one container runtime source.
type ContainerSource struct {
	Name    string // docker | podman | cgroup
	Scraper scrape.Scraper
}

// ContainerChain scrapes the sources in order (Docker Engine API → Podman → cgroup v2, contracts
// §22) and keeps only the first source's samples for a container id seen by several sources.
// The scrape fails only when every source failed.
type ContainerChain struct {
	Sources []ContainerSource
}

// Close implements scrape.Scraper.
func (c *ContainerChain) Close() error {
	var errs []error
	for _, s := range c.Sources {
		errs = append(errs, s.Scraper.Close())
	}
	return errors.Join(errs...)
}

// Scrape implements scrape.Scraper.
func (c *ContainerChain) Scrape(ctx context.Context) (emit.Result, error) {
	var out emit.Result
	seen := map[string]string{} // container id → source
	var errs []error
	for _, s := range c.Sources {
		r, err := s.Scraper.Scrape(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.Name, err))
			continue
		}
		mine := map[string]bool{}
		keep := func(samples []emit.Sample) []emit.Sample {
			var k []emit.Sample
			for _, x := range samples {
				id := ""
				if v, ok := x.Attrs.Value(emit.AttrContainerID); ok {
					id = v.AsString()
				}
				if src, dup := seen[id]; dup && src != s.Name && id != "" {
					continue
				}
				mine[id] = true
				k = append(k, x)
			}
			return k
		}
		out.Gauges = append(out.Gauges, keep(r.Gauges)...)
		out.Counts = append(out.Counts, keep(r.Counts)...)
		for id := range mine {
			seen[id] = s.Name
		}
	}
	if len(errs) == len(c.Sources) && len(errs) > 0 {
		return out, errors.Join(errs...)
	}
	return out, nil
}

// ---------------------------------------------------------------- Podman

// PodmanSockets returns the Podman Docker-compatible socket candidates: TT_AGENT_PODMAN_SOCKET,
// else rootful /run/podman/podman.sock, rootless $XDG_RUNTIME_DIR/podman/podman.sock, and the
// same paths under the host root (agent in a container) incl. every user's
// /run/user/<uid>/podman/podman.sock.
func PodmanSockets(env string, fsys HostFS) []string {
	if env != "" {
		return []string{env}
	}
	out := []string{"/run/podman/podman.sock"}
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		out = append(out, filepath.Join(x, "podman", "podman.sock"))
	}
	if fsys.Root != "" {
		out = append(out, filepath.Join(fsys.Root, "run/podman/podman.sock"))
	}
	if m, _ := filepath.Glob(fsys.root("/run/user/*/podman/podman.sock")); len(m) > 0 {
		out = append(out, m...)
	}
	return out
}

// ---------------------------------------------------------------- cgroup v2

// CgroupScraper reports trooper.container.* for any runtime (containerd/nerdctl, CRI-O, LXC,
// systemd-nspawn, …) straight from the cgroup v2 tree: cpu.stat, memory.current, memory.max,
// cpu.max, memory.stat. The container id is the 64-hex id in the cgroup directory name; name
// and image are unknown (name = first 12 characters of the id). Linux only.
type CgroupScraper struct {
	Root     string // /sys/fs/cgroup (or <TT_AGENT_SYSFS>/fs/cgroup)
	MaxDepth int
	counters *rate.Counters
	now      func() time.Time
}

// NewCgroupScraper reads the unified hierarchy at root.
func NewCgroupScraper(root string) *CgroupScraper {
	return &CgroupScraper{Root: root, MaxDepth: 8, counters: rate.NewCounters(), now: time.Now}
}

// CgroupRoot is the cgroup v2 mount for a HostFS.
func (fs HostFS) CgroupRoot() string {
	if fs.Sys != "" {
		return filepath.Join(fs.Sys, "fs", "cgroup")
	}
	return "/sys/fs/cgroup"
}

// CgroupV2 reports whether root is a cgroup v2 (unified) hierarchy.
func CgroupV2(root string) bool {
	_, err := os.Stat(filepath.Join(root, "cgroup.controllers"))
	return err == nil
}

// Close implements scrape.Scraper.
func (c *CgroupScraper) Close() error { return nil }

// cgroupID matches a container cgroup directory: docker-<id>.scope, cri-containerd-<id>.scope,
// crio-<id>.scope, libpod-<id>.scope, nerdctl-<id>.scope, or a bare <id> (containerd namespaces).
var cgroupID = regexp.MustCompile(`^(?:[a-z0-9-]+-)?([0-9a-f]{64})(?:\.scope)?$`)

// Scrape implements scrape.Scraper.
func (c *CgroupScraper) Scrape(ctx context.Context) (emit.Result, error) {
	var r emit.Result
	if !CgroupV2(c.Root) {
		return r, fmt.Errorf("%s is not a cgroup v2 hierarchy", c.Root)
	}
	now := c.now()
	base := strings.Count(filepath.Clean(c.Root), string(filepath.Separator))
	err := filepath.WalkDir(c.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if strings.Count(p, string(filepath.Separator))-base > c.MaxDepth {
			return filepath.SkipDir
		}
		m := cgroupID.FindStringSubmatch(d.Name())
		if m == nil {
			return nil
		}
		c.container(&r, p, m[1], now)
		return filepath.SkipDir // nested cgroups (libpod-…/container) belong to the same container
	})
	c.counters.Prune(now.Add(-5 * time.Minute))
	return r, err
}

func (c *CgroupScraper) container(r *emit.Result, dir, id string, now time.Time) {
	stat := readKV(filepath.Join(dir, "cpu.stat"))
	if len(stat) == 0 {
		return // not a leaf with controllers (or gone)
	}
	name := id[:12]
	attrs := ContainerAttrs(id, name, "", "", "", "", "", "", name)
	if v, ok := stat["usage_usec"]; ok {
		if cores, ok := c.counters.Rate(id+"/cpu", v/1e6, now); ok {
			r.Gauge(emit.ContainerCPU, cores, attrs...)
		}
	}
	if cur, ok := readNumber(filepath.Join(dir, "memory.current")); ok {
		inactive := readKV(filepath.Join(dir, "memory.stat"))["inactive_file"]
		r.Gauge(emit.ContainerMemoryBytes, max(0, cur-inactive), attrs...)
	}
	if lim, ok := readNumber(filepath.Join(dir, "memory.max")); ok { // "max" = unlimited → no sample
		r.Gauge(emit.ContainerMemoryLimitBytes, lim, attrs...)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "cpu.max")); err == nil {
		if f := strings.Fields(string(b)); len(f) == 2 && f[0] != "max" {
			q, err1 := strconv.ParseFloat(f[0], 64)
			p, err2 := strconv.ParseFloat(f[1], 64)
			if err1 == nil && err2 == nil && p > 0 {
				r.Gauge(emit.ContainerCPULimit, q/p, attrs...)
			}
		}
	}
	dp, _, ok1 := c.counters.Delta(id+"/periods", stat["nr_periods"], now)
	dt, _, ok2 := c.counters.Delta(id+"/throttled", stat["nr_throttled"], now)
	if ok1 && ok2 {
		v := 0.0
		if dp > 0 {
			v = rate.Clamp01(dt / dp)
		}
		r.Gauge(emit.ContainerThrottled, v, attrs...)
	}
}

// readKV parses "key value" lines (cpu.stat, memory.stat).
func readKV(p string) map[string]float64 {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]float64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			out[k] = n
		}
	}
	return out
}

// readNumber reads a single-number file; "max" → false.
func readNumber(p string) (float64, bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	return v, err == nil
}
