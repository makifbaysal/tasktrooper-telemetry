package flatten

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

const infraTS = 1_700_000_040_000_000_000 // 22:34:00 UTC, minute 1_700_000_040

func igauge(name string, v float64, ts uint64, a []*commonpb.KeyValue) *metricspb.Metric {
	return &metricspb.Metric{Name: name, Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
		TimeUnixNano: ts, Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: v}, Attributes: a}}}}}
}

func imetrics(res []*commonpb.KeyValue, ms ...*metricspb.Metric) *colmetricspb.ExportMetricsServiceRequest {
	return &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: res}, ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: ms}}}}}
}

// col returns the value of column c of row r (by InfraColumns position).
func col(t *testing.T, r *InfraRow, c string) any {
	t.Helper()
	cols := strings.Split(InfraColumns(r.Fam), ", ")
	vals := r.Values()
	if len(cols) != len(vals) {
		t.Fatalf("%s: %d columns, %d values", InfraTable(r.Fam), len(cols), len(vals))
	}
	for i, name := range cols {
		if name == c {
			return vals[i]
		}
	}
	t.Fatalf("%s has no column %q", InfraTable(r.Fam), c)
	return nil
}

func only(t *testing.T, b *MetricsBatch, f Family) *InfraRow {
	t.Helper()
	if len(b.Infra[f]) != 1 {
		t.Fatalf("%s: %d rows, want 1", InfraTable(f), len(b.Infra[f]))
	}
	for _, r := range b.Infra[f] {
		return r
	}
	return nil
}

func TestInfraRouting(t *testing.T) {
	agentRes := attrs("service.name", "trooper-agent", "host.name", "node-a", "host.id", "m-1", "os.type", "linux", "host.arch", "amd64",
		"k8s.cluster.name", "prod", "k8s.node.name", "node-a")
	ctr := attrs("container.id", "c1", "container.name", "api", "container.image.name", "ghcr.io/x/api:1", "k8s.namespace.name", "shop",
		"k8s.pod.name", "api-7f", "k8s.pod.uid", "u1", "trooper.k8s.workload.kind", "Deployment", "trooper.k8s.workload.name", "api",
		"trooper.service", "orders-api")
	pod := func(phase, reason string) []*commonpb.KeyValue {
		return attrs("k8s.namespace.name", "shop", "k8s.pod.name", "api-7f", "k8s.pod.uid", "u1", "k8s.pod.phase", phase,
			"trooper.k8s.pod.reason", reason, "trooper.k8s.workload.kind", "Deployment", "trooper.k8s.workload.name", "api", "trooper.service", "orders-api")
	}
	wl := attrs("k8s.namespace.name", "shop", "trooper.k8s.workload.kind", "Deployment", "trooper.k8s.workload.name", "api")
	long := strings.Repeat("x", 300)
	b := NewMetricsBatch()
	b.Add(tn, imetrics(agentRes,
		igauge("trooper.host.cpu", 0.4, infraTS, nil),
		igauge("trooper.host.cpu", 0.7, infraTS+15e9, nil), // same minute → max
		igauge("trooper.host.memory_bytes", 2e9, infraTS, nil),
		igauge("trooper.container.cpu", 0.25, infraTS, ctr),
		igauge("trooper.container.memory_limit_bytes", 512e6, infraTS, ctr),
		igauge("trooper.process.cpu", 1.5, infraTS, attrs("process.pid", 4242, "process.executable.name", "java", "process.command", long, "container.id", "c1")),
		igauge("trooper.k8s.node.ready", 1, infraTS, nil),
		igauge("trooper.k8s.pod.restarts", 4, infraTS+30e9, pod("Running", "CrashLoopBackOff")),
		igauge("trooper.k8s.pod.ready", 0, infraTS, pod("Running", "")), // older point: does not override the state
		igauge("trooper.k8s.workload.desired", 3, infraTS, wl),
		igauge("trooper.k8s.workload.available", 1, infraTS, wl),
		igauge("trooper.host.unknown", 1, infraTS, nil), // unknown name → custom metrics
	))
	b.Add(tn, imetrics(attrs("service.name", "trooper-cloud", "cloud.provider", "aws", "cloud.region", "eu-west-1"),
		igauge("trooper.faas.invocations", 120, infraTS, attrs("trooper.faas.id", "aws-eu-west-1-resize", "faas.name", "resize")),
		igauge("trooper.faas.duration.p95", 830, infraTS, attrs("trooper.faas.id", "aws-eu-west-1-resize", "faas.name", "resize")),
	))
	minute := time.Unix(1_700_000_040, 0).UTC()

	h := only(t, b, FamHosts)
	for c, want := range map[string]any{"cluster": "prod", "host": "node-a", "host_id": "m-1", "os": "linux", "arch": "amd64", "node": "node-a",
		"minute": minute, "cpu": 0.7, "memory_bytes": 2e9} {
		if got := col(t, h, c); got != want {
			t.Errorf("hosts_1m.%s = %v, want %v", c, got, want)
		}
	}
	if v := col(t, h, "disk").(float64); !math.IsInf(v, -1) {
		t.Errorf("unreported disk = %v, want -Inf", v)
	}
	c := only(t, b, FamContainers)
	for k, want := range map[string]any{"cluster": "prod", "host": "node-a", "namespace": "shop", "pod": "api-7f", "pod_uid": "u1",
		"container": "api", "container_id": "c1", "image": "ghcr.io/x/api:1", "workload_kind": "Deployment", "workload": "api",
		"service": "orders-api", "cpu": 0.25, "memory_limit": 512e6} {
		if got := col(t, c, k); got != want {
			t.Errorf("containers_1m.%s = %v, want %v", k, got, want)
		}
	}
	p := only(t, b, FamProcesses)
	if col(t, p, "pid") != uint32(4242) || col(t, p, "exe") != "java" || len(col(t, p, "command").(string)) != maxCommandLen || col(t, p, "cpu") != 1.5 {
		t.Errorf("processes_1m: %v", p.Values())
	}
	if n := only(t, b, FamK8sNodes); col(t, n, "cluster") != "prod" || col(t, n, "node") != "node-a" || col(t, n, "ready") != 1.0 {
		t.Errorf("k8s_nodes_1m: %v", n.Values())
	}
	// pod: points with different phase/reason attrs have different keys only through dims; phase/reason are state
	pr := only(t, b, FamK8sPods)
	if col(t, pr, "reason") != "CrashLoopBackOff" || col(t, pr, "phase") != "Running" || col(t, pr, "ready") != 0.0 || col(t, pr, "restarts") != 4.0 ||
		col(t, pr, "node") != "node-a" || col(t, pr, "service") != "orders-api" {
		t.Errorf("k8s_pods_1m: %v", pr.Values())
	}
	w := only(t, b, FamK8sWorkloads)
	if col(t, w, "kind") != "Deployment" || col(t, w, "name") != "api" || col(t, w, "desired") != 3.0 || col(t, w, "available") != 1.0 ||
		!math.IsInf(col(t, w, "ready").(float64), -1) {
		t.Errorf("k8s_workloads_1m: %v", w.Values())
	}
	f := only(t, b, FamFaaS)
	if col(t, f, "function_id") != "aws-eu-west-1-resize" || col(t, f, "provider") != "aws" || col(t, f, "region") != "eu-west-1" ||
		col(t, f, "invocations") != 120.0 || col(t, f, "duration_p95") != 830.0 || !math.IsInf(col(t, f, "errors").(float64), -1) {
		t.Errorf("faas_1m: %v", f.Values())
	}
	if len(b.Metrics) != 1 || b.Metrics[0].Name != "trooper.host.unknown" {
		t.Errorf("custom metrics: %+v", b.Metrics)
	}
	if b.Len() != 8 {
		t.Errorf("Len = %d, want 8", b.Len())
	}
	b.Reset()
	if b.Len() != 0 {
		t.Errorf("Reset: Len = %d", b.Len())
	}
}

func TestInfraKeys(t *testing.T) {
	cases := []struct {
		name string
		res  []*commonpb.KeyValue
		ms   []*metricspb.Metric
		fam  Family
		rows int
	}{
		{"two hosts", attrs("host.name", "a"), []*metricspb.Metric{
			igauge("trooper.host.cpu", 1, infraTS, nil), igauge("trooper.host.cpu", 1, infraTS, attrs("host.name", "b"))}, FamHosts, 2},
		{"two minutes", attrs("host.name", "a"), []*metricspb.Metric{
			igauge("trooper.host.cpu", 1, infraTS, nil), igauge("trooper.host.cpu", 1, infraTS+60e9, nil)}, FamHosts, 2},
		{"same-named docker containers on two hosts", nil, []*metricspb.Metric{
			igauge("trooper.container.cpu", 1, infraTS, attrs("host.name", "a", "container.name", "redis")),
			igauge("trooper.container.cpu", 1, infraTS, attrs("host.name", "b", "container.name", "redis"))}, FamContainers, 2},
		{"metrics of one pod merge", attrs("k8s.cluster.name", "c"), []*metricspb.Metric{
			igauge("trooper.k8s.pod.ready", 1, infraTS, attrs("k8s.pod.uid", "u", "trooper.k8s.pod.reason", "")),
			igauge("trooper.k8s.pod.cpu_limit", 1, infraTS, attrs("k8s.pod.uid", "u", "trooper.k8s.pod.reason", "OOMKilled"))}, FamK8sPods, 1},
		{"functions by id", attrs("cloud.provider", "azure"), []*metricspb.Metric{
			igauge("trooper.faas.invocations", 1, infraTS, attrs("trooper.faas.id", "azure-a")),
			igauge("trooper.faas.execution_units", 1, infraTS, attrs("trooper.faas.id", "azure-b"))}, FamFaaS, 2},
		{"gpus of one host by index", attrs("host.name", "gpu-1"), []*metricspb.Metric{
			igauge("trooper.host.gpu.utilization", 1, infraTS, attrs("gpu.index", 0, "gpu.uuid", "GPU-a", "gpu.model", "A100")),
			igauge("trooper.host.gpu.memory", 1, infraTS, attrs("gpu.index", 0, "gpu.uuid", "GPU-a", "gpu.model", "A100")),
			igauge("trooper.host.gpu.utilization", 1, infraTS, attrs("gpu.index", 1, "gpu.uuid", "GPU-b", "gpu.model", "A100"))}, FamGPUs, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewMetricsBatch()
			b.Add(tn, imetrics(tc.res, tc.ms...))
			if len(b.Infra[tc.fam]) != tc.rows || b.Len() != tc.rows {
				t.Errorf("rows = %d (Len %d), want %d", len(b.Infra[tc.fam]), b.Len(), tc.rows)
			}
		})
	}
}

func TestInfraColumnsMatchSchema(t *testing.T) {
	want := map[Family]string{
		FamHosts:        "org_id, project_id, cluster, host, host_id, os, arch, node, minute, cpu, cpu_cores, memory, memory_bytes, disk, disk_read, disk_write, net_in, net_out, load1, uptime_s",
		FamContainers:   "org_id, project_id, cluster, host, namespace, pod, pod_uid, container, container_id, image, workload_kind, workload, service, minute, cpu, cpu_limit, memory_bytes, memory_limit, restarts, net_in, net_out, throttled",
		FamProcesses:    "org_id, project_id, host, container_id, pid, exe, command, minute, cpu, memory_bytes, threads, fds",
		FamK8sNodes:     "org_id, project_id, cluster, node, minute, ready, allocatable_cpu, allocatable_memory, pods",
		FamK8sPods:      "org_id, project_id, cluster, namespace, pod, pod_uid, node, workload_kind, workload, service, minute, phase, reason, ready, restarts, cpu_request, cpu_limit, memory_request, memory_limit",
		FamK8sWorkloads: "org_id, project_id, cluster, namespace, kind, name, minute, desired, ready, available, updated",
		FamFaaS:         "org_id, project_id, function_id, provider, region, minute, invocations, errors, throttles, duration_avg, duration_max, duration_p95, duration_p99, concurrency, iterator_age_ms, memory_bytes, execution_units",
		FamGPUs:         "org_id, project_id, cluster, host, gpu_index, gpu_uuid, model, minute, utilization, memory, memory_bytes, temperature_c, power_w",
	}
	for f, cols := range want {
		if got := InfraColumns(f); got != cols {
			t.Errorf("%s:\n got %s\nwant %s", InfraTable(f), got, cols)
		}
	}
}

// GPU gauges (contracts §22) land in gpus_1m with a UInt8 gpu_index; hosts_1m is untouched.
func TestGPURouting(t *testing.T) {
	res := attrs("service.name", "trooper-agent", "host.name", "gpu-node", "k8s.cluster.name", "prod")
	g0 := attrs("gpu.index", 1, "gpu.uuid", "GPU-5f1c", "gpu.model", "NVIDIA A100-SXM4-40GB")
	b := NewMetricsBatch()
	b.Add(tn, imetrics(res,
		igauge("trooper.host.gpu.utilization", 0.42, infraTS, g0),
		igauge("trooper.host.gpu.utilization", 0.91, infraTS+15e9, g0),
		igauge("trooper.host.gpu.memory", 0.5, infraTS, g0),
		igauge("trooper.host.gpu.memory_bytes", 20e9, infraTS, g0),
		igauge("trooper.host.gpu.temperature_c", 64, infraTS, g0),
		igauge("trooper.host.gpu.power_w", 250, infraTS, g0),
	))
	if len(b.Infra[FamHosts]) != 0 {
		t.Errorf("gpu metrics reached hosts_1m")
	}
	g := only(t, b, FamGPUs)
	for c, want := range map[string]any{"cluster": "prod", "host": "gpu-node", "gpu_index": uint8(1), "gpu_uuid": "GPU-5f1c",
		"model": "NVIDIA A100-SXM4-40GB", "minute": time.Unix(1_700_000_040, 0).UTC(), "utilization": 0.91, "memory": 0.5,
		"memory_bytes": 20e9, "temperature_c": 64.0, "power_w": 250.0} {
		if got := col(t, g, c); got != want {
			t.Errorf("gpus_1m.%s = %v (%T), want %v", c, got, got, want)
		}
	}
	if InfraTable(FamGPUs) != "gpus_1m" {
		t.Errorf("table %s", InfraTable(FamGPUs))
	}
}

// Kubernetes Warning events (trooper-agent cluster mode) keep their attributes and get the resource's cluster.
func TestK8sEventLog(t *testing.T) {
	req := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: attrs("service.name", "k8s", "k8s.cluster.name", "prod")},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			TimeUnixNano: infraTS, SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_WARN,
			Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "Back-off restarting failed container"}},
			Attributes: attrs("k8s.event.reason", "BackOff", "k8s.event.type", "Warning", "k8s.object.kind", "Pod",
				"k8s.object.name", "api-7f", "k8s.namespace.name", "shop", "k8s.event.count", 5),
		}}}},
	}}}
	r := Logs(tn, req, time.Now(), nil)[0]
	if r.Service != "k8s" || r.Severity != "warn" {
		t.Errorf("service/severity: %s %s", r.Service, r.Severity)
	}
	for k, v := range map[string]string{"k8s.event.reason": "BackOff", "k8s.event.type": "Warning", "k8s.object.kind": "Pod",
		"k8s.object.name": "api-7f", "k8s.namespace.name": "shop", "k8s.event.count": "5", "k8s.cluster.name": "prod"} {
		if r.Attrs[k] != v {
			t.Errorf("attrs[%s] = %q, want %q", k, r.Attrs[k], v)
		}
	}
}

// OBI / Beyla spans keep telemetry.sdk.name in resource_attrs (api: source 'ebpf').
func TestEBPFSpanResource(t *testing.T) {
	req := traces("checkout", span(1, 0, server, attrs("http.route", "/pay")))
	req.ResourceSpans[0].Resource.Attributes = append(req.ResourceSpans[0].Resource.Attributes,
		kv("telemetry.sdk.name", "opentelemetry-ebpf-instrumentation"))
	r := Spans(tn, req, nil)[0]
	if r.ResourceAttrs["telemetry.sdk.name"] != "opentelemetry-ebpf-instrumentation" || r.Service != "checkout" || r.IsEntry != 1 {
		t.Errorf("ebpf span: service=%s entry=%v res=%v", r.Service, r.IsEntry, r.ResourceAttrs)
	}
}

// BenchmarkInfraMetrics: one host-mode export (host gauges, 20 containers × 8, 20 processes × 4) + one cluster-mode
// export (20 pods × 6, 5 workloads × 4) + 2 GPUs × 5 → typed rows.
func BenchmarkInfraMetrics(b *testing.B) {
	res := attrs("service.name", "trooper-agent", "host.name", "node-a", "host.id", "m-1", "os.type", "linux", "host.arch", "amd64",
		"k8s.cluster.name", "prod", "k8s.node.name", "node-a")
	var ms []*metricspb.Metric
	for _, n := range []string{"cpu", "cpu_cores", "memory", "memory_bytes", "disk", "disk_read", "disk_write", "net_in", "net_out", "load1", "uptime_s"} {
		ms = append(ms, igauge("trooper.host."+n, 0.5, infraTS, nil))
	}
	for i := range 20 {
		a := attrs("container.id", fmt.Sprintf("c%d", i), "container.name", "app", "container.image.name", "img:1", "k8s.namespace.name", "shop",
			"k8s.pod.name", fmt.Sprintf("app-%d", i), "k8s.pod.uid", fmt.Sprintf("u%d", i), "trooper.k8s.workload.kind", "Deployment",
			"trooper.k8s.workload.name", "app", "trooper.service", "app")
		for _, n := range []string{"cpu", "cpu_limit", "memory_bytes", "memory_limit_bytes", "restarts", "net_in", "net_out", "throttled"} {
			ms = append(ms, igauge("trooper.container."+n, 1, infraTS, a))
		}
		pa := attrs("process.pid", 100+i, "process.executable.name", "java", "process.command", "java", "container.id", fmt.Sprintf("c%d", i))
		for _, n := range []string{"cpu", "memory_bytes", "threads", "fds"} {
			ms = append(ms, igauge("trooper.process."+n, 1, infraTS, pa))
		}
		po := attrs("k8s.namespace.name", "shop", "k8s.pod.name", fmt.Sprintf("app-%d", i), "k8s.pod.uid", fmt.Sprintf("u%d", i), "k8s.node.name", "node-a",
			"k8s.pod.phase", "Running", "trooper.k8s.pod.reason", "", "trooper.k8s.workload.kind", "Deployment", "trooper.k8s.workload.name", "app", "trooper.service", "app")
		for _, n := range []string{"ready", "restarts", "cpu_request", "cpu_limit", "memory_request", "memory_limit"} {
			ms = append(ms, igauge("trooper.k8s.pod."+n, 1, infraTS, po))
		}
	}
	for i := range 5 {
		wa := attrs("k8s.namespace.name", "shop", "trooper.k8s.workload.kind", "Deployment", "trooper.k8s.workload.name", fmt.Sprintf("w%d", i))
		for _, n := range []string{"desired", "ready", "available", "updated"} {
			ms = append(ms, igauge("trooper.k8s.workload."+n, 1, infraTS, wa))
		}
	}
	for i := range 2 {
		ga := attrs("gpu.index", i, "gpu.uuid", fmt.Sprintf("GPU-%d", i), "gpu.model", "NVIDIA A100")
		for _, n := range []string{"utilization", "memory", "memory_bytes", "temperature_c", "power_w"} {
			ms = append(ms, igauge("trooper.host.gpu."+n, 1, infraTS, ga))
		}
	}
	req := imetrics(res, ms...)
	batch := NewMetricsBatch()
	b.ReportAllocs()
	for b.Loop() {
		batch.Reset()
		batch.Add(tn, req)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(ms)), "ns/point")
}
