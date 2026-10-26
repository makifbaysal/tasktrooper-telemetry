package scrape

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/rate"
)

// PromSample is one sample of the Prometheus text exposition format.
type PromSample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// PromSamples is a parsed scrape, indexed by metric name (lower-cased: JMX exporter configs differ
// in case, e.g. kafka_server_BrokerTopicMetrics_BytesInPerSec_Count vs …_bytesin_total).
type PromSamples map[string][]PromSample

// ParsePromText parses the Prometheus text format (and the OpenMetrics subset exporters emit).
// Comments, timestamps and malformed lines are skipped.
func ParsePromText(r io.Reader) (PromSamples, error) {
	out := PromSamples{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		s, ok := parsePromLine(line)
		if !ok {
			continue
		}
		key := strings.ToLower(s.Name)
		out[key] = append(out[key], s)
	}
	return out, sc.Err()
}

func parsePromLine(line string) (PromSample, bool) {
	s := PromSample{}
	i := strings.IndexAny(line, "{ \t")
	if i <= 0 {
		return s, false
	}
	s.Name = line[:i]
	rest := line[i:]
	if rest[0] == '{' {
		labels, n, ok := parsePromLabels(rest)
		if !ok {
			return s, false
		}
		s.Labels = labels
		rest = rest[n:]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return s, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return s, false
	}
	s.Value = v
	return s, true
}

// parsePromLabels parses `{a="x",b="y\"z"}` and returns the labels and the bytes consumed.
func parsePromLabels(s string) (map[string]string, int, bool) {
	labels := map[string]string{}
	i := 1
	for i < len(s) {
		for i < len(s) && (s[i] == ' ' || s[i] == ',') {
			i++
		}
		if i < len(s) && s[i] == '}' {
			return labels, i + 1, true
		}
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			return nil, 0, false
		}
		name := strings.TrimSpace(s[i : i+eq])
		i += eq + 1
		if i >= len(s) || s[i] != '"' {
			return nil, 0, false
		}
		i++
		var b strings.Builder
		for i < len(s) && s[i] != '"' {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					b.WriteByte('\n')
				default:
					b.WriteByte(s[i])
				}
			} else {
				b.WriteByte(s[i])
			}
			i++
		}
		if i >= len(s) {
			return nil, 0, false
		}
		labels[name] = b.String()
		i++ // closing quote
	}
	return nil, 0, false
}

// Sum adds the values of name's samples that match all of the given label=value filters
// ("label!=" style negation via a leading '!' in the value).
func (p PromSamples) Sum(name string, filters ...string) (float64, bool) {
	var sum float64
	found := false
	for _, s := range p[strings.ToLower(name)] {
		if matchLabels(s.Labels, filters) && !math.IsNaN(s.Value) {
			sum += s.Value
			found = true
		}
	}
	return sum, found
}

// First returns the first value of any of the names (gauges such as JMX CpuLoad).
func (p PromSamples) First(names ...string) (float64, bool) {
	for _, n := range names {
		if ss := p[strings.ToLower(n)]; len(ss) > 0 && !math.IsNaN(ss[0].Value) {
			return ss[0].Value, true
		}
	}
	return 0, false
}

func matchLabels(l map[string]string, filters []string) bool {
	for _, f := range filters {
		k, v, _ := strings.Cut(f, "=")
		if strings.HasPrefix(v, "!") {
			if l[k] == v[1:] {
				return false
			}
			continue
		}
		if l[k] != v {
			return false
		}
	}
	return true
}

// NodeValues are the trooper.broker.node.* values derived from one Prometheus scrape.
type NodeValues map[string]float64

// NodeMapper turns node exporter / Kafka JMX exporter / Redpanda / rabbitmq_prometheus samples into
// trooper.broker.node.{cpu,memory,disk,net_in}. Counters become rates between two scrapes, so the
// first scrape of a node reports only the gauge-based values. Sources, first match wins:
//
//	cpu     node_cpu_seconds_total (1 − Δ(idle+iowait)/Δall) · redpanda_cpu_busy_seconds_total (Δ/s/shards) ·
//	        java_lang_OperatingSystem_CpuLoad|SystemCpuLoad (JMX, 0..1) · process_cpu_seconds_total (Δ/s/available
//	        processors) · rabbitmq: Erlang scheduler utilisation from erlang_vm_msacc_* (needs microstate accounting)
//	memory  1 − node_memory_MemAvailable/MemTotal · redpanda allocated/(allocated+free) ·
//	        JMX 1 − FreeMemorySize/TotalMemorySize (or *PhysicalMemorySize) · rabbitmq resident/limit (watermark)
//	disk    node_filesystem 1 − avail/size on diskMount · redpanda 1 − storage_disk_free/total ·
//	        rabbitmq disk_free_limit/disk_free (proximity to the free-disk alarm)
//	net_in  MB/s: Δnode_network_receive_bytes_total (no lo) · Δkafka_server_BrokerTopicMetrics BytesIn ·
//	        Δredpanda_kafka_request_bytes_total{redpanda_request="produce"} · Δerlang_vm_statistics_bytes_received_total
type NodeMapper struct {
	c *rate.Counters
}

// NewNodeMapper returns a mapper with its own counter state.
func NewNodeMapper() *NodeMapper { return &NodeMapper{c: rate.NewCounters()} }

// Map derives the node values; key separates the counter state of different nodes.
func (m *NodeMapper) Map(key string, p PromSamples, diskMount string, now time.Time) NodeValues {
	v := NodeValues{}
	if cpu, ok := m.cpu(key, p, now); ok {
		v[emit.BrokerNodeCPU] = rate.Clamp01(cpu)
	}
	if mem, ok := memoryFrom(p); ok {
		v[emit.BrokerNodeMemory] = rate.Clamp01(mem)
	}
	if d, ok := diskFrom(p, diskMount); ok {
		v[emit.BrokerNodeDisk] = rate.Clamp01(d)
	}
	if n, ok := m.netIn(key, p, now); ok {
		v[emit.BrokerNodeNetIn] = n
	}
	m.c.Prune(now.Add(-time.Hour))
	return v
}

func (m *NodeMapper) cpu(key string, p PromSamples, now time.Time) (float64, bool) {
	if all, ok := p.Sum("node_cpu_seconds_total"); ok {
		idle, _ := p.Sum("node_cpu_seconds_total", "mode=idle")
		iow, _ := p.Sum("node_cpu_seconds_total", "mode=iowait")
		dAll, _, ok1 := m.c.Delta(key+"/cpu/all", all, now)
		dIdle, _, ok2 := m.c.Delta(key+"/cpu/idle", idle+iow, now)
		if ok1 && ok2 && dAll > 0 {
			return 1 - dIdle/dAll, true
		}
		return 0, false
	}
	if busy, ok := p.Sum("redpanda_cpu_busy_seconds_total"); ok {
		shards := float64(len(p["redpanda_cpu_busy_seconds_total"]))
		if r, ok := m.c.Rate(key+"/cpu/rp", busy, now); ok && shards > 0 {
			return r / shards, true
		}
		return 0, false
	}
	if l, ok := p.First("java_lang_operatingsystem_cpuload", "java_lang_operatingsystem_systemcpuload"); ok && l >= 0 {
		return l, true
	}
	if pc, ok := p.Sum("process_cpu_seconds_total"); ok {
		if n, ok := p.First("java_lang_operatingsystem_availableprocessors", "erlang_vm_logical_processors_available"); ok && n > 0 {
			if r, ok := m.c.Rate(key+"/cpu/proc", pc, now); ok {
				return r / n, true
			}
		}
		return 0, false
	}
	// rabbitmq_prometheus: scheduler microstate accounting (all zero unless enabled)
	var all, idle float64
	for name, ss := range p {
		if !strings.HasPrefix(name, "erlang_vm_msacc_") || !strings.HasSuffix(name, "_seconds_total") {
			continue
		}
		for _, s := range ss {
			if s.Labels["type"] != "scheduler" {
				continue
			}
			all += s.Value
			if name == "erlang_vm_msacc_sleep_seconds_total" || name == "erlang_vm_msacc_busy_wait_seconds_total" {
				idle += s.Value
			}
		}
	}
	if all > 0 {
		dAll, _, ok1 := m.c.Delta(key+"/cpu/msacc", all, now)
		dIdle, _, ok2 := m.c.Delta(key+"/cpu/msacc_idle", idle, now)
		if ok1 && ok2 && dAll > 0 {
			return 1 - dIdle/dAll, true
		}
	}
	return 0, false
}

func memoryFrom(p PromSamples) (float64, bool) {
	if t, ok := p.First("node_memory_memtotal_bytes"); ok && t > 0 {
		if a, ok := p.First("node_memory_memavailable_bytes"); ok {
			return 1 - a/t, true
		}
	}
	if a, ok := p.Sum("redpanda_memory_allocated_memory"); ok {
		f, _ := p.Sum("redpanda_memory_free_memory")
		if a+f > 0 {
			return a / (a + f), true
		}
	}
	for _, pair := range [][2]string{
		{"java_lang_operatingsystem_totalmemorysize", "java_lang_operatingsystem_freememorysize"},
		{"java_lang_operatingsystem_totalphysicalmemorysize", "java_lang_operatingsystem_freephysicalmemorysize"},
	} {
		if t, ok := p.First(pair[0]); ok && t > 0 {
			if f, ok := p.First(pair[1]); ok {
				return 1 - f/t, true
			}
		}
	}
	if used, ok := p.First("rabbitmq_process_resident_memory_bytes"); ok {
		if lim, ok := p.First("rabbitmq_resident_memory_limit_bytes"); ok && lim > 0 {
			return used / lim, true
		}
	}
	return 0, false
}

func diskFrom(p PromSamples, mount string) (float64, bool) {
	if mount == "" {
		mount = "/"
	}
	if size, ok := p.Sum("node_filesystem_size_bytes", "mountpoint="+mount); ok && size > 0 {
		if avail, ok := p.Sum("node_filesystem_avail_bytes", "mountpoint="+mount); ok {
			return 1 - avail/size, true
		}
	}
	if t, ok := p.Sum("redpanda_storage_disk_total_bytes"); ok && t > 0 {
		if f, ok := p.Sum("redpanda_storage_disk_free_bytes"); ok {
			return 1 - f/t, true
		}
	}
	if free, ok := p.First("rabbitmq_disk_space_available_bytes"); ok && free > 0 {
		if lim, ok := p.First("rabbitmq_disk_space_available_limit_bytes"); ok {
			return lim / free, true
		}
	}
	return 0, false
}

// kafkaBytesIn returns the broker-wide BytesIn counter of a JMX exporter scrape.
func kafkaBytesIn(p PromSamples) (float64, bool) {
	for name, ss := range p {
		if !strings.HasPrefix(name, "kafka_server_brokertopicmetrics_bytesin") ||
			!(strings.HasSuffix(name, "_total") || strings.HasSuffix(name, "_count")) {
			continue
		}
		var agg, perTopic float64
		haveAgg := false
		for _, s := range ss {
			if s.Labels["topic"] == "" {
				agg += s.Value
				haveAgg = true
			} else {
				perTopic += s.Value
			}
		}
		if haveAgg {
			return agg, true
		}
		return perTopic, true
	}
	return 0, false
}

func (m *NodeMapper) netIn(key string, p PromSamples, now time.Time) (float64, bool) {
	var total float64
	var ok bool
	if total, ok = p.Sum("node_network_receive_bytes_total", "device=!lo"); !ok {
		if total, ok = kafkaBytesIn(p); !ok {
			if total, ok = p.Sum("redpanda_kafka_request_bytes_total", "redpanda_request=produce"); !ok {
				total, ok = p.First("erlang_vm_statistics_bytes_received_total")
			}
		}
	}
	if !ok {
		return 0, false
	}
	r, ok := m.c.Rate(key+"/net_in", total, now)
	return r / 1e6, ok
}

// PromNode is one configured Prometheus endpoint.
type PromNode struct {
	Name, URL, DiskMount string
}

// promClient fetches Prometheus endpoints.
type promClient struct {
	hc *http.Client
}

func newPromClient(timeout time.Duration, insecure bool) *promClient {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in
	}
	return &promClient{hc: &http.Client{Timeout: timeout, Transport: tr}}
}

func (c *promClient) fetch(ctx context.Context, u string) (PromSamples, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return ParsePromText(io.LimitReader(resp.Body, 64<<20))
}

// hostOf returns the host of a URL (default node name).
func hostOf(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return u
}
