package emit

import (
	"slices"
	"testing"
)

// Golden list from .ai/contracts.md §20 (the ingest writer routes exactly these names to the
// hosts_1m / containers_1m / processes_1m / k8s_*_1m tables). A rename here breaks ingestion.
var contract20 = []string{
	"trooper.host.cpu", "trooper.host.memory", "trooper.host.memory_bytes", "trooper.host.disk",
	"trooper.host.disk_read", "trooper.host.disk_write", "trooper.host.net_in", "trooper.host.net_out",
	"trooper.host.load1", "trooper.host.uptime_s", "trooper.host.cpu_cores",
	"trooper.container.cpu", "trooper.container.cpu_limit", "trooper.container.memory_bytes", "trooper.container.memory_limit_bytes",
	"trooper.container.restarts", "trooper.container.net_in", "trooper.container.net_out", "trooper.container.throttled",
	"trooper.process.cpu", "trooper.process.memory_bytes", "trooper.process.threads", "trooper.process.fds",
	"trooper.k8s.node.ready", "trooper.k8s.node.allocatable_cpu", "trooper.k8s.node.allocatable_memory", "trooper.k8s.node.pods",
	"trooper.k8s.pod.ready", "trooper.k8s.pod.restarts", "trooper.k8s.pod.cpu_request", "trooper.k8s.pod.cpu_limit",
	"trooper.k8s.pod.memory_request", "trooper.k8s.pod.memory_limit",
	"trooper.k8s.workload.desired", "trooper.k8s.workload.ready", "trooper.k8s.workload.available", "trooper.k8s.workload.updated",
}

func TestInfraNamesMatchContract(t *testing.T) {
	got := slices.Sorted(slices.Values(InfraGaugeNames))
	want := slices.Sorted(slices.Values(contract20))
	if !slices.Equal(got, want) {
		t.Fatalf("§20 metric names drifted:\n got  %v\n want %v", got, want)
	}
	for _, n := range contract20 {
		if !slices.Contains(GaugeNames, n) {
			t.Errorf("%s is not registered as a gauge", n)
		}
	}
}

func TestInfraAttrKeysMatchContract(t *testing.T) {
	tests := []struct{ got, want string }{
		{ResHostName, "host.name"}, {ResHostID, "host.id"}, {ResOSType, "os.type"}, {ResHostArch, "host.arch"},
		{ResK8sCluster, "k8s.cluster.name"}, {ResK8sNode, "k8s.node.name"}, {ResCloudProvider, "cloud.provider"}, {ResCloudRegion, "cloud.region"},
		{AttrContainerID, "container.id"}, {AttrContainerName, "container.name"}, {AttrContainerImage, "container.image.name"},
		{AttrK8sNamespace, "k8s.namespace.name"}, {AttrK8sPod, "k8s.pod.name"}, {AttrK8sPodUID, "k8s.pod.uid"},
		{AttrK8sNode, "k8s.node.name"}, {AttrK8sPodPhase, "k8s.pod.phase"}, {AttrK8sPodReason, "trooper.k8s.pod.reason"},
		{AttrWorkloadKind, "trooper.k8s.workload.kind"}, {AttrWorkloadName, "trooper.k8s.workload.name"}, {AttrService, "trooper.service"},
		{AttrProcessPID, "process.pid"}, {AttrProcessExe, "process.executable.name"}, {AttrProcessCommand, "process.command"},
		{AttrEventReason, "k8s.event.reason"}, {AttrEventType, "k8s.event.type"}, {AttrObjectKind, "k8s.object.kind"},
		{AttrObjectName, "k8s.object.name"}, {AttrEventCount, "k8s.event.count"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("attr key %q, contract says %q", tt.got, tt.want)
		}
	}
}

// Golden list from .ai/contracts.md §22 (GPU → gpus_1m).
func TestGPUNamesMatchContract(t *testing.T) {
	want := []string{"trooper.host.gpu.utilization", "trooper.host.gpu.memory", "trooper.host.gpu.memory_bytes",
		"trooper.host.gpu.temperature_c", "trooper.host.gpu.power_w"}
	if !slices.Equal(GPUGaugeNames, want) {
		t.Fatalf("GPU names drifted: %v", GPUGaugeNames)
	}
	for _, n := range want {
		if !slices.Contains(GaugeNames, n) {
			t.Errorf("%s is not registered as a gauge", n)
		}
	}
	if AttrGPUIndex != "gpu.index" || AttrGPUModel != "gpu.model" || AttrGPUUUID != "gpu.uuid" {
		t.Fatal("GPU attr keys drifted")
	}
}
