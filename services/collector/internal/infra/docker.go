package infra

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/rate"
)

// DockerScraper reports trooper.container.* from the Docker Engine API over its unix socket
// (read-only GETs: /containers/json, /containers/{id}/json, /containers/{id}/stats). Used
// outside Kubernetes.
type DockerScraper struct {
	hc       *http.Client
	base     string
	counters *rate.Counters
	now      func() time.Time
	// Parallel bounds concurrent per-container calls.
	Parallel int
}

// NewDockerScraper talks to the engine on socket.
func NewDockerScraper(socket string) *DockerScraper {
	tr := &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return dialEngine(ctx, socket) },
		MaxIdleConnsPerHost: 8,
	}
	return &DockerScraper{hc: &http.Client{Transport: tr, Timeout: 30 * time.Second}, base: "http://docker", counters: rate.NewCounters(), now: time.Now, Parallel: 8}
}

// Close implements scrape.Scraper.
func (d *DockerScraper) Close() error {
	d.hc.CloseIdleConnections()
	return nil
}

func (d *DockerScraper) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("docker GET %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type dockerListItem struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
}

type dockerInspect struct {
	RestartCount int `json:"RestartCount"`
	HostConfig   struct {
		Memory    int64 `json:"Memory"`
		NanoCpus  int64 `json:"NanoCpus"`
		CPUQuota  int64 `json:"CpuQuota"`
		CPUPeriod int64 `json:"CpuPeriod"`
	} `json:"HostConfig"`
}

type dockerStats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage float64 `json:"total_usage"` // ns
		} `json:"cpu_usage"`
		Throttling struct {
			Periods          float64 `json:"periods"`
			ThrottledPeriods float64 `json:"throttled_periods"`
		} `json:"throttling_data"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage float64            `json:"usage"`
		Stats map[string]float64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes float64 `json:"rx_bytes"`
		TxBytes float64 `json:"tx_bytes"`
	} `json:"networks"`
}

// DockerServiceLabel is the container label that sets trooper.service (else the container name).
const DockerServiceLabel = "tasktrooper.io/service"

// Scrape implements scrape.Scraper.
func (d *DockerScraper) Scrape(ctx context.Context) (emit.Result, error) {
	var r emit.Result
	var list []dockerListItem
	if err := d.get(ctx, "/containers/json", &list); err != nil {
		return r, err
	}
	now := d.now()
	type one struct {
		res emit.Result
	}
	results := make([]one, len(list))
	sem := make(chan struct{}, max(1, d.Parallel))
	var wg sync.WaitGroup
	for i, c := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i].res = d.container(ctx, c, now)
		}()
	}
	wg.Wait()
	for _, x := range results {
		r.Merge(x.res)
	}
	d.counters.Prune(now.Add(-5 * time.Minute))
	return r, nil
}

func (d *DockerScraper) container(ctx context.Context, c dockerListItem, now time.Time) emit.Result {
	var r emit.Result
	var st dockerStats
	if err := d.get(ctx, "/containers/"+url.PathEscape(c.ID)+"/stats?stream=false&one-shot=true", &st); err != nil {
		return r // container stopped between the calls
	}
	var in dockerInspect
	haveInspect := d.get(ctx, "/containers/"+url.PathEscape(c.ID)+"/json", &in) == nil
	name := c.ID
	if len(c.Names) > 0 {
		name = strings.TrimPrefix(c.Names[0], "/")
	}
	svc := c.Labels[DockerServiceLabel]
	if svc == "" {
		svc = name
	}
	attrs := ContainerAttrs(c.ID, name, c.Image, "", "", "", "", "", svc)
	if v, ok := d.counters.Rate(c.ID+"/cpu", st.CPUStats.CPUUsage.TotalUsage/1e9, now); ok {
		r.Gauge(emit.ContainerCPU, v, attrs...)
	}
	if st.MemoryStats.Usage > 0 {
		inactive := st.MemoryStats.Stats["inactive_file"] // cgroup v2
		if v, ok := st.MemoryStats.Stats["total_inactive_file"]; ok {
			inactive = v // cgroup v1
		}
		r.Gauge(emit.ContainerMemoryBytes, max(0, st.MemoryStats.Usage-inactive), attrs...)
	}
	if haveInspect {
		hc := in.HostConfig
		if hc.NanoCpus > 0 {
			r.Gauge(emit.ContainerCPULimit, float64(hc.NanoCpus)/1e9, attrs...)
		} else if hc.CPUQuota > 0 {
			period := hc.CPUPeriod
			if period <= 0 {
				period = 100000
			}
			r.Gauge(emit.ContainerCPULimit, float64(hc.CPUQuota)/float64(period), attrs...)
		}
		if hc.Memory > 0 {
			r.Gauge(emit.ContainerMemoryLimitBytes, float64(hc.Memory), attrs...)
		}
		r.Gauge(emit.ContainerRestarts, float64(in.RestartCount), attrs...)
	}
	if len(st.Networks) > 0 {
		var rx, tx float64
		for _, n := range st.Networks {
			rx += n.RxBytes
			tx += n.TxBytes
		}
		if v, ok := d.counters.Rate(c.ID+"/rx", rx, now); ok {
			r.Gauge(emit.ContainerNetIn, v, attrs...)
		}
		if v, ok := d.counters.Rate(c.ID+"/tx", tx, now); ok {
			r.Gauge(emit.ContainerNetOut, v, attrs...)
		}
	}
	dp, _, ok1 := d.counters.Delta(c.ID+"/periods", st.CPUStats.Throttling.Periods, now)
	dt, _, ok2 := d.counters.Delta(c.ID+"/throttled", st.CPUStats.Throttling.ThrottledPeriods, now)
	if ok1 && ok2 {
		v := 0.0
		if dp > 0 {
			v = rate.Clamp01(dt / dp)
		}
		r.Gauge(emit.ContainerThrottled, v, attrs...)
	}
	return r
}
