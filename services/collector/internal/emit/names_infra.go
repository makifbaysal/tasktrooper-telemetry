package emit

// Host, container, process and Kubernetes metrics (contracts §20, ADR-019). All gauges.
const (
	HostCPU         = "trooper.host.cpu"          // 0..1
	HostCPUCores    = "trooper.host.cpu_cores"    //
	HostMemory      = "trooper.host.memory"       // 0..1
	HostMemoryBytes = "trooper.host.memory_bytes" // byte (used)
	HostDisk        = "trooper.host.disk"         // 0..1, fullest filesystem
	HostDiskRead    = "trooper.host.disk_read"    // byte/s
	HostDiskWrite   = "trooper.host.disk_write"   // byte/s
	HostNetIn       = "trooper.host.net_in"       // byte/s
	HostNetOut      = "trooper.host.net_out"      // byte/s
	HostLoad1       = "trooper.host.load1"        //
	HostUptimeS     = "trooper.host.uptime_s"     // s

	// GPU (contracts §22, nvidia-smi; table gpus_1m)
	HostGPUUtilization = "trooper.host.gpu.utilization"   // 0..1
	HostGPUMemory      = "trooper.host.gpu.memory"        // 0..1
	HostGPUMemoryBytes = "trooper.host.gpu.memory_bytes"  // byte (used)
	HostGPUTemperature = "trooper.host.gpu.temperature_c" // °C
	HostGPUPower       = "trooper.host.gpu.power_w"       // W

	ContainerCPU              = "trooper.container.cpu"                // core
	ContainerCPULimit         = "trooper.container.cpu_limit"          // core
	ContainerMemoryBytes      = "trooper.container.memory_bytes"       // byte
	ContainerMemoryLimitBytes = "trooper.container.memory_limit_bytes" // byte
	ContainerRestarts         = "trooper.container.restarts"           // cumulative count
	ContainerNetIn            = "trooper.container.net_in"             // byte/s
	ContainerNetOut           = "trooper.container.net_out"            // byte/s
	ContainerThrottled        = "trooper.container.throttled"          // 0..1

	ProcessCPU         = "trooper.process.cpu"          // core
	ProcessMemoryBytes = "trooper.process.memory_bytes" // byte (RSS)
	ProcessThreads     = "trooper.process.threads"      //
	ProcessFDs         = "trooper.process.fds"          //

	K8sNodeReady             = "trooper.k8s.node.ready"              // 0/1
	K8sNodeAllocatableCPU    = "trooper.k8s.node.allocatable_cpu"    // core
	K8sNodeAllocatableMemory = "trooper.k8s.node.allocatable_memory" // byte
	K8sNodePods              = "trooper.k8s.node.pods"               //

	K8sPodReady         = "trooper.k8s.pod.ready"          // 0/1
	K8sPodRestarts      = "trooper.k8s.pod.restarts"       // cumulative count
	K8sPodCPURequest    = "trooper.k8s.pod.cpu_request"    // core
	K8sPodCPULimit      = "trooper.k8s.pod.cpu_limit"      // core
	K8sPodMemoryRequest = "trooper.k8s.pod.memory_request" // byte
	K8sPodMemoryLimit   = "trooper.k8s.pod.memory_limit"   // byte

	K8sWorkloadDesired   = "trooper.k8s.workload.desired"
	K8sWorkloadReady     = "trooper.k8s.workload.ready"
	K8sWorkloadAvailable = "trooper.k8s.workload.available"
	K8sWorkloadUpdated   = "trooper.k8s.workload.updated"
)

// Resource and data point attribute keys (contracts §20).
const (
	ResHostName      = "host.name"
	ResHostID        = "host.id"
	ResOSType        = "os.type"
	ResHostArch      = "host.arch"
	ResK8sCluster    = "k8s.cluster.name"
	ResK8sNode       = "k8s.node.name"
	ResCloudProvider = "cloud.provider"
	ResCloudRegion   = "cloud.region"

	AttrContainerID    = "container.id"
	AttrContainerName  = "container.name"
	AttrContainerImage = "container.image.name"
	AttrK8sNamespace   = "k8s.namespace.name"
	AttrK8sPod         = "k8s.pod.name"
	AttrK8sPodUID      = "k8s.pod.uid"
	AttrK8sNode        = "k8s.node.name"
	AttrK8sPodPhase    = "k8s.pod.phase"
	AttrK8sPodReason   = "trooper.k8s.pod.reason"
	AttrWorkloadKind   = "trooper.k8s.workload.kind"
	AttrWorkloadName   = "trooper.k8s.workload.name"
	AttrService        = "trooper.service"

	AttrProcessPID     = "process.pid"
	AttrProcessExe     = "process.executable.name"
	AttrProcessCommand = "process.command"

	AttrGPUIndex = "gpu.index"
	AttrGPUModel = "gpu.model"
	AttrGPUUUID  = "gpu.uuid"

	// Warning event log attributes (cluster mode).
	AttrEventReason = "k8s.event.reason"
	AttrEventType   = "k8s.event.type"
	AttrObjectKind  = "k8s.object.kind"
	AttrObjectName  = "k8s.object.name"
	AttrEventCount  = "k8s.event.count"
)

// GPUGaugeNames lists the §22 GPU gauges.
var GPUGaugeNames = []string{HostGPUUtilization, HostGPUMemory, HostGPUMemoryBytes, HostGPUTemperature, HostGPUPower}

// InfraGaugeNames lists the §20 gauges (registered by every emitter, like GaugeNames).
var InfraGaugeNames = []string{
	HostCPU, HostCPUCores, HostMemory, HostMemoryBytes, HostDisk, HostDiskRead, HostDiskWrite, HostNetIn, HostNetOut,
	HostLoad1, HostUptimeS,
	ContainerCPU, ContainerCPULimit, ContainerMemoryBytes, ContainerMemoryLimitBytes, ContainerRestarts,
	ContainerNetIn, ContainerNetOut, ContainerThrottled,
	ProcessCPU, ProcessMemoryBytes, ProcessThreads, ProcessFDs,
	K8sNodeReady, K8sNodeAllocatableCPU, K8sNodeAllocatableMemory, K8sNodePods,
	K8sPodReady, K8sPodRestarts, K8sPodCPURequest, K8sPodCPULimit, K8sPodMemoryRequest, K8sPodMemoryLimit,
	K8sWorkloadDesired, K8sWorkloadReady, K8sWorkloadAvailable, K8sWorkloadUpdated,
}

func init() {
	GaugeNames = append(GaugeNames, InfraGaugeNames...)
	GaugeNames = append(GaugeNames, GPUGaugeNames...)
	for k, v := range map[string]string{
		HostGPUUtilization: "1", HostGPUMemory: "1", HostGPUMemoryBytes: "By", HostGPUTemperature: "Cel", HostGPUPower: "W",
		HostCPU: "1", HostCPUCores: "{cpu}", HostMemory: "1", HostMemoryBytes: "By", HostDisk: "1",
		HostDiskRead: "By/s", HostDiskWrite: "By/s", HostNetIn: "By/s", HostNetOut: "By/s", HostUptimeS: "s",
		ContainerCPU: "{cpu}", ContainerCPULimit: "{cpu}", ContainerMemoryBytes: "By", ContainerMemoryLimitBytes: "By",
		ContainerNetIn: "By/s", ContainerNetOut: "By/s", ContainerThrottled: "1",
		ProcessCPU: "{cpu}", ProcessMemoryBytes: "By",
		K8sNodeAllocatableCPU: "{cpu}", K8sNodeAllocatableMemory: "By",
		K8sPodCPURequest: "{cpu}", K8sPodCPULimit: "{cpu}", K8sPodMemoryRequest: "By", K8sPodMemoryLimit: "By",
	} {
		units[k] = v
	}
}
