package flatten

import (
	"math"
	"strconv"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"

	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// Host / container / process / Kubernetes / serverless / GPU families (contracts §20–§22;
// deploy/clickhouse/003_infra_k8s_faas.sql, 004_gpu.sql). Every family is one AggregatingMergeTree table keyed by its dimension
// columns + minute; gauges are merged with max inside a flush, unreported value columns are -Inf (contracts §5) and
// state columns (pod phase / reason) keep the value of the newest data point.

// Family is one of the typed infra tables.
type Family uint8

const (
	FamHosts Family = iota
	FamContainers
	FamProcesses
	FamK8sNodes
	FamK8sPods
	FamK8sWorkloads
	FamFaaS
	FamGPUs
	numFamilies
)

const (
	maxInfraDims   = 11
	maxInfraStates = 2
	maxInfraVals   = 11
	maxCommandLen  = 128
)

type infraDim struct {
	col   string
	attrs []string // first non-empty attribute (data point first, then resource)
	limit int      // max length (0 = MaxAttrValueLen)
	uint  int      // 8 / 32 = UInt8 / UInt32 column parsed from the attribute (0 = String)
}

type familySpec struct {
	table  string
	dims   []infraDim
	states []infraDim
	vals   []string
}

func d(col string, attrs ...string) infraDim { return infraDim{col: col, attrs: attrs} }

var (
	dCluster   = d("cluster", "k8s.cluster.name")
	dHost      = d("host", "host.name")
	dNamespace = d("namespace", "k8s.namespace.name")
	dPod       = d("pod", "k8s.pod.name")
	dPodUID    = d("pod_uid", "k8s.pod.uid")
	dNode      = d("node", "k8s.node.name")
	dWlKind    = d("workload_kind", "trooper.k8s.workload.kind")
	dWorkload  = d("workload", "trooper.k8s.workload.name")
	dService   = d("service", "trooper.service")
)

var families = [numFamilies]familySpec{
	FamHosts: {table: "hosts_1m",
		dims: []infraDim{dCluster, dHost, d("host_id", "host.id"), d("os", "os.type"), d("arch", "host.arch"), dNode},
		vals: []string{"cpu", "cpu_cores", "memory", "memory_bytes", "disk", "disk_read", "disk_write", "net_in", "net_out", "load1", "uptime_s"}},
	FamContainers: {table: "containers_1m",
		dims: []infraDim{dCluster, dHost, dNamespace, dPod, dPodUID, d("container", "container.name"), d("container_id", "container.id"),
			d("image", "container.image.name"), dWlKind, dWorkload, dService},
		vals: []string{"cpu", "cpu_limit", "memory_bytes", "memory_limit", "restarts", "net_in", "net_out", "throttled"}},
	FamProcesses: {table: "processes_1m",
		dims: []infraDim{dHost, d("container_id", "container.id"), {col: "pid", attrs: []string{"process.pid"}, uint: 32}, d("exe", "process.executable.name"),
			{col: "command", attrs: []string{"process.command"}, limit: maxCommandLen}},
		vals: []string{"cpu", "memory_bytes", "threads", "fds"}},
	FamK8sNodes: {table: "k8s_nodes_1m",
		dims: []infraDim{dCluster, dNode},
		vals: []string{"ready", "allocatable_cpu", "allocatable_memory", "pods"}},
	FamK8sPods: {table: "k8s_pods_1m",
		dims:   []infraDim{dCluster, dNamespace, dPod, dPodUID, dNode, dWlKind, dWorkload, dService},
		states: []infraDim{d("phase", "k8s.pod.phase"), d("reason", "trooper.k8s.pod.reason")},
		vals:   []string{"ready", "restarts", "cpu_request", "cpu_limit", "memory_request", "memory_limit"}},
	FamK8sWorkloads: {table: "k8s_workloads_1m",
		dims: []infraDim{dCluster, dNamespace, d("kind", "trooper.k8s.workload.kind"), d("name", "trooper.k8s.workload.name")},
		vals: []string{"desired", "ready", "available", "updated"}},
	FamFaaS: {table: "faas_1m",
		dims: []infraDim{d("function_id", "trooper.faas.id"), d("provider", "cloud.provider"), d("region", "cloud.region")},
		vals: []string{"invocations", "errors", "throttles", "duration_avg", "duration_max", "duration_p95", "duration_p99",
			"concurrency", "iterator_age_ms", "memory_bytes", "execution_units"}},
	FamGPUs: {table: "gpus_1m",
		dims: []infraDim{dCluster, dHost, {col: "gpu_index", attrs: []string{"gpu.index"}, uint: 8}, d("gpu_uuid", "gpu.uuid"), d("model", "gpu.model")},
		vals: []string{"utilization", "memory", "memory_bytes", "temperature_c", "power_w"}},
}

type infraCol struct {
	fam Family
	col uint8
}

// infraMetricCol maps an OTLP metric name to its family and value column.
var infraMetricCol = func() map[string]infraCol {
	names := map[Family]map[string]string{
		FamHosts: {"trooper.host.cpu": "cpu", "trooper.host.cpu_cores": "cpu_cores", "trooper.host.memory": "memory",
			"trooper.host.memory_bytes": "memory_bytes", "trooper.host.disk": "disk", "trooper.host.disk_read": "disk_read",
			"trooper.host.disk_write": "disk_write", "trooper.host.net_in": "net_in", "trooper.host.net_out": "net_out",
			"trooper.host.load1": "load1", "trooper.host.uptime_s": "uptime_s"},
		FamContainers: {"trooper.container.cpu": "cpu", "trooper.container.cpu_limit": "cpu_limit",
			"trooper.container.memory_bytes": "memory_bytes", "trooper.container.memory_limit_bytes": "memory_limit",
			"trooper.container.restarts": "restarts", "trooper.container.net_in": "net_in", "trooper.container.net_out": "net_out",
			"trooper.container.throttled": "throttled"},
		FamProcesses: {"trooper.process.cpu": "cpu", "trooper.process.memory_bytes": "memory_bytes",
			"trooper.process.threads": "threads", "trooper.process.fds": "fds"},
		FamK8sNodes: {"trooper.k8s.node.ready": "ready", "trooper.k8s.node.allocatable_cpu": "allocatable_cpu",
			"trooper.k8s.node.allocatable_memory": "allocatable_memory", "trooper.k8s.node.pods": "pods"},
		FamK8sPods: {"trooper.k8s.pod.ready": "ready", "trooper.k8s.pod.restarts": "restarts",
			"trooper.k8s.pod.cpu_request": "cpu_request", "trooper.k8s.pod.cpu_limit": "cpu_limit",
			"trooper.k8s.pod.memory_request": "memory_request", "trooper.k8s.pod.memory_limit": "memory_limit"},
		FamK8sWorkloads: {"trooper.k8s.workload.desired": "desired", "trooper.k8s.workload.ready": "ready",
			"trooper.k8s.workload.available": "available", "trooper.k8s.workload.updated": "updated"},
		FamFaaS: {"trooper.faas.invocations": "invocations", "trooper.faas.errors": "errors", "trooper.faas.throttles": "throttles",
			"trooper.faas.duration.avg": "duration_avg", "trooper.faas.duration.max": "duration_max",
			"trooper.faas.duration.p95": "duration_p95", "trooper.faas.duration.p99": "duration_p99",
			"trooper.faas.concurrency": "concurrency", "trooper.faas.iterator_age_ms": "iterator_age_ms",
			"trooper.faas.memory_bytes": "memory_bytes", "trooper.faas.execution_units": "execution_units"},
		FamGPUs: {"trooper.host.gpu.utilization": "utilization", "trooper.host.gpu.memory": "memory",
			"trooper.host.gpu.memory_bytes": "memory_bytes", "trooper.host.gpu.temperature_c": "temperature_c",
			"trooper.host.gpu.power_w": "power_w"},
	}
	out := map[string]infraCol{}
	for fam, m := range names {
		for metric, col := range m {
			i := -1
			for j, v := range families[fam].vals {
				if v == col {
					i = j
				}
			}
			if i < 0 {
				panic("flatten: unknown infra column " + col)
			}
			out[metric] = infraCol{fam, uint8(i)}
		}
	}
	return out
}()

// InfraColumns returns the INSERT column list of a family (matches InfraRow.Values).
func InfraColumns(f Family) string { return infraColumns[f] }

// InfraTable returns the ClickHouse table of a family.
func InfraTable(f Family) string { return families[f].table }

var infraColumns = func() [numFamilies]string {
	var out [numFamilies]string
	for f, s := range families {
		cols := []string{"org_id", "project_id"}
		for _, d := range s.dims {
			cols = append(cols, d.col)
		}
		cols = append(cols, "minute")
		for _, d := range s.states {
			cols = append(cols, d.col)
		}
		out[f] = strings.Join(append(cols, s.vals...), ", ")
	}
	return out
}()

// InfraKey identifies one row: family dimensions + minute.
type InfraKey struct {
	Org, Project uint32
	Dims         [maxInfraDims]string
	Minute       int64
}

// InfraRow is one row of an infra family table.
type InfraRow struct {
	Fam     Family
	Key     InfraKey
	State   [maxInfraStates]string
	stateTS uint64 // timestamp of the data point State was taken from (newest wins)
	V       [maxInfraVals]float64
}

// Values returns the row in InfraColumns(r.Fam) order.
func (r *InfraRow) Values() []any {
	s := &families[r.Fam]
	v := make([]any, 0, 3+len(s.dims)+len(s.states)+len(s.vals))
	v = append(v, r.Key.Org, r.Key.Project)
	for i, dim := range s.dims {
		switch dim.uint {
		case 32:
			n, _ := strconv.ParseUint(r.Key.Dims[i], 10, 32)
			v = append(v, uint32(n))
		case 8:
			n, _ := strconv.ParseUint(r.Key.Dims[i], 10, 8)
			v = append(v, uint8(n))
		default:
			v = append(v, r.Key.Dims[i])
		}
	}
	v = append(v, time.Unix(r.Key.Minute, 0).UTC())
	for i := range s.states {
		v = append(v, r.State[i])
	}
	for i := range s.vals {
		v = append(v, r.V[i])
	}
	return v
}

// addInfra routes a host/container/process/k8s/faas metric; false if the name is not one of them.
func (b *MetricsBatch) addInfra(t tenant.Tenant, name string, m *metricspb.Metric, resKV []*commonpb.KeyValue) bool {
	ic, ok := infraMetricCol[name]
	if !ok {
		return false
	}
	s := &families[ic.fam]
	rows := b.Infra[ic.fam]
	if rows == nil {
		rows = map[InfraKey]*InfraRow{}
		b.Infra[ic.fam] = rows
	}
	for _, dp := range numberPoints(m) {
		a := pair{attrIndex(dp.GetAttributes()), attrIndex(resKV)}
		k := InfraKey{Org: t.OrgID, Project: t.ProjectID, Minute: minuteOf(dp.GetTimeUnixNano())}
		for i, dim := range s.dims {
			k.Dims[i] = a.firstOf(dim.attrs, dim.limit)
		}
		r := rows[k]
		if r == nil {
			r = &InfraRow{Fam: ic.fam, Key: k}
			for i := range r.V {
				r.V[i] = unsetMax
			}
			rows[k] = r
		}
		if len(s.states) > 0 && dp.GetTimeUnixNano() >= r.stateTS {
			r.stateTS = dp.GetTimeUnixNano()
			for i, st := range s.states {
				r.State[i] = a.firstOf(st.attrs, st.limit)
			}
		}
		if v := numberValue(dp); !math.IsNaN(v) {
			r.V[ic.col] = math.Max(r.V[ic.col], v)
		}
	}
	return true
}

func (p pair) firstOf(keys []string, limit int) string {
	if limit <= 0 {
		limit = MaxAttrValueLen
	}
	for _, k := range keys {
		if s := p.str(k); s != "" {
			return capValue(s, limit)
		}
	}
	return ""
}
