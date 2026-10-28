package infra

import (
	"bufio"
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/rate"
)

// statsSummary is the subset of the kubelet's /stats/summary we read.
type statsSummary struct {
	Pods []struct {
		PodRef struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			UID       string `json:"uid"`
		} `json:"podRef"`
		Containers []struct {
			Name string `json:"name"`
			CPU  *struct {
				UsageNanoCores       *float64 `json:"usageNanoCores"`
				UsageCoreNanoSeconds *float64 `json:"usageCoreNanoSeconds"`
			} `json:"cpu"`
			Memory *struct {
				WorkingSetBytes *float64 `json:"workingSetBytes"`
			} `json:"memory"`
		} `json:"containers"`
		Network *struct {
			RxBytes *float64 `json:"rxBytes"`
			TxBytes *float64 `json:"txBytes"`
		} `json:"network"`
	} `json:"pods"`
}

// KubeletScraper reports trooper.container.* for the pods on this node from the kubelet
// (/stats/summary for usage, /pods for metadata, limits and restarts, /metrics/cadvisor for CPU
// throttling — optional, skipped when not permitted).
type KubeletScraper struct {
	C        *KubeClient
	counters *rate.Counters
	// throttling is disabled after a 401/403/404 on /metrics/cadvisor (re-tried every 10 min)
	noCadvisorUntil time.Time
	now             func() time.Time
}

// NewKubeletScraper builds the scraper.
func NewKubeletScraper(c *KubeClient) *KubeletScraper {
	return &KubeletScraper{C: c, counters: rate.NewCounters(), now: time.Now}
}

// Close implements scrape.Scraper.
func (k *KubeletScraper) Close() error { return nil }

// Scrape implements scrape.Scraper.
func (k *KubeletScraper) Scrape(ctx context.Context) (emit.Result, error) {
	var r emit.Result
	now := k.now()
	var podList struct {
		Items []Pod `json:"items"`
	}
	if err := k.C.Get(ctx, "/pods", &podList); err != nil {
		return r, err
	}
	var sum statsSummary
	if err := k.C.Get(ctx, "/stats/summary", &sum); err != nil {
		return r, err
	}
	throttle := k.throttling(ctx, now)

	pods := make(map[string]*Pod, len(podList.Items))
	for i := range podList.Items {
		pods[podList.Items[i].Metadata.UID] = &podList.Items[i]
	}
	owners := Owners{} // host mode: name heuristics (cluster mode resolves via the API)
	seen := map[string]bool{}
	for _, ps := range sum.Pods {
		pod := pods[ps.PodRef.UID]
		if pod == nil {
			continue // pod deleted between the two calls
		}
		kind, wl := owners.Workload(pod.Metadata)
		svc := ServiceOf(pod.Metadata.Labels)
		netDone := false
		for _, cs := range ps.Containers {
			spec, _ := findSpec(pod, cs.Name)
			st, _ := pod.ContainerStatus(cs.Name)
			id := StripContainerID(st.ContainerID)
			image := spec.Image
			if image == "" {
				image = st.Image
			}
			attrs := ContainerAttrs(id, cs.Name, image, pod.Metadata.Namespace, pod.Metadata.Name, pod.Metadata.UID, kind, wl, svc)
			key := pod.Metadata.UID + "/" + cs.Name
			seen[key] = true
			if cs.CPU != nil {
				if cs.CPU.UsageNanoCores != nil {
					r.Gauge(emit.ContainerCPU, *cs.CPU.UsageNanoCores/1e9, attrs...)
				} else if cs.CPU.UsageCoreNanoSeconds != nil {
					if v, ok := k.counters.Rate(key+"/cpu", *cs.CPU.UsageCoreNanoSeconds/1e9, now); ok {
						r.Gauge(emit.ContainerCPU, v, attrs...)
					}
				}
			}
			if cs.Memory != nil && cs.Memory.WorkingSetBytes != nil {
				r.Gauge(emit.ContainerMemoryBytes, *cs.Memory.WorkingSetBytes, attrs...)
			}
			if v, ok := ParseQuantity(spec.Resources.Limits["cpu"]); ok {
				r.Gauge(emit.ContainerCPULimit, v, attrs...)
			}
			if v, ok := ParseQuantity(spec.Resources.Limits["memory"]); ok {
				r.Gauge(emit.ContainerMemoryLimitBytes, v, attrs...)
			}
			r.Gauge(emit.ContainerRestarts, float64(st.RestartCount), attrs...)
			if v, ok := throttle[pod.Metadata.Namespace+"/"+pod.Metadata.Name+"/"+cs.Name]; ok {
				r.Gauge(emit.ContainerThrottled, v, attrs...)
			}
			// Pod network is shared by its containers: reported once, on the first container
			// (sums stay right; the other containers have no network sample).
			if !netDone && ps.Network != nil && ps.Network.RxBytes != nil && ps.Network.TxBytes != nil {
				netDone = true
				if v, ok := k.counters.Rate(pod.Metadata.UID+"/rx", *ps.Network.RxBytes, now); ok {
					r.Gauge(emit.ContainerNetIn, v, attrs...)
				}
				if v, ok := k.counters.Rate(pod.Metadata.UID+"/tx", *ps.Network.TxBytes, now); ok {
					r.Gauge(emit.ContainerNetOut, v, attrs...)
				}
			}
		}
	}
	k.counters.Prune(now.Add(-5 * time.Minute))
	return r, nil
}

func findSpec(p *Pod, name string) (ContainerSpec, bool) {
	for _, c := range p.Spec.Containers {
		if c.Name == name {
			return c, true
		}
	}
	return ContainerSpec{}, false
}

// ContainerAttrs are the attributes of every trooper.container.* gauge.
func ContainerAttrs(id, name, image, ns, pod, podUID, kind, workload, service string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(emit.AttrContainerID, id),
		attribute.String(emit.AttrContainerName, name),
		attribute.String(emit.AttrContainerImage, image),
		attribute.String(emit.AttrK8sNamespace, ns),
		attribute.String(emit.AttrK8sPod, pod),
		attribute.String(emit.AttrK8sPodUID, podUID),
		attribute.String(emit.AttrWorkloadKind, kind),
		attribute.String(emit.AttrWorkloadName, workload),
		attribute.String(emit.AttrService, service),
	}
}

// throttling returns Δthrottled_periods / Δperiods per "<ns>/<pod>/<container>" from
// /metrics/cadvisor. Errors disable it for 10 minutes (it is optional).
func (k *KubeletScraper) throttling(ctx context.Context, now time.Time) map[string]float64 {
	out := map[string]float64{}
	if now.Before(k.noCadvisorUntil) {
		return out
	}
	body, err := k.C.Open(ctx, "/metrics/cadvisor")
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) || ctx.Err() == nil {
			k.noCadvisorUntil = now.Add(10 * time.Minute)
		}
		return out
	}
	defer body.Close()
	periods, throttled := map[string]float64{}, map[string]float64{}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		var dst map[string]float64
		switch {
		case strings.HasPrefix(line, "container_cpu_cfs_throttled_periods_total{"):
			dst = throttled
		case strings.HasPrefix(line, "container_cpu_cfs_periods_total{"):
			dst = periods
		default:
			continue
		}
		labels, val, ok := parsePromLine(line)
		if !ok || labels["container"] == "" || labels["pod"] == "" {
			continue
		}
		dst[labels["namespace"]+"/"+labels["pod"]+"/"+labels["container"]] = val
	}
	for key, p := range periods {
		t, ok := throttled[key]
		if !ok {
			continue
		}
		dp, _, ok1 := k.counters.Delta(key+"/periods", p, now)
		dthr, _, ok2 := k.counters.Delta(key+"/throttled", t, now)
		if ok1 && ok2 {
			if dp > 0 {
				out[key] = rate.Clamp01(dthr / dp)
			} else {
				out[key] = 0
			}
		}
	}
	return out
}

// parsePromLine parses `name{a="b",c="d"} 1.5 [ts]`.
func parsePromLine(line string) (map[string]string, float64, bool) {
	open := strings.IndexByte(line, '{')
	end := strings.LastIndexByte(line, '}')
	if open < 0 || end < open {
		return nil, 0, false
	}
	labels := map[string]string{}
	rest := line[open+1 : end]
	for rest != "" {
		eq := strings.IndexByte(rest, '=')
		if eq < 0 || eq+1 >= len(rest) || rest[eq+1] != '"' {
			break
		}
		name := strings.TrimSpace(rest[:eq])
		i := eq + 2
		var sb strings.Builder
		for i < len(rest) && rest[i] != '"' {
			if rest[i] == '\\' && i+1 < len(rest) {
				i++
			}
			sb.WriteByte(rest[i])
			i++
		}
		labels[name] = sb.String()
		rest = strings.TrimPrefix(strings.TrimSpace(rest[min(i+1, len(rest)):]), ",")
	}
	fields := strings.Fields(line[end+1:])
	if len(fields) == 0 {
		return nil, 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	return labels, v, err == nil
}
