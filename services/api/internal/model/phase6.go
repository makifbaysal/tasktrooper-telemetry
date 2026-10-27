package model

// Faz 6 shapes: AI usage quota (ADR-018, contracts §19), hosts / containers / Kubernetes (ADR-019, §20) and
// serverless functions (ADR-020, §21). Detail responses carry the list row's fields unchanged; their series are
// under `series` and their lists are named `*List`.

// ---------------------------------------------------------------- AI usage (§19)

// AIOrgUsage: Limit / Remaining nil = unlimited.
type AIOrgUsage struct {
	Limit     *float64 `json:"limit"`
	Used      float64  `json:"used"`
	Remaining *float64 `json:"remaining"`
}

// AILimitUsed is a limit (nil = none / unlimited) and the credits used against it.
type AILimitUsed struct {
	Limit *float64 `json:"limit"`
	Used  float64  `json:"used"`
}

type AIUserUsage struct {
	UserID string   `json:"userId"`
	Email  string   `json:"email"`
	Name   string   `json:"name"`
	Limit  *float64 `json:"limit"`
	Used   float64  `json:"used"`
}

type AIFeatureUsage struct {
	Feature string  `json:"feature"`
	Credits float64 `json:"credits"`
	Calls   int64   `json:"calls"`
}

type AIProjectUsage struct {
	ProjectID string  `json:"projectId"`
	Name      string  `json:"name"`
	Credits   float64 `json:"credits"`
}

// AIUsage is GET /usage/ai.
type AIUsage struct {
	Period    string           `json:"period"`   // YYYY-MM (UTC calendar month)
	ResetsAt  int64            `json:"resetsAt"` // unix ms of the next month's start
	Plan      string           `json:"plan"`
	Org       AIOrgUsage       `json:"org"`
	Me        AILimitUsed      `json:"me"`
	ByUser    []AIUserUsage    `json:"byUser"` // admin+ only; [] otherwise
	ByFeature []AIFeatureUsage `json:"byFeature"`
	ByProject []AIProjectUsage `json:"byProject"`
	Daily     SeriesSet        `json:"daily"` // one bucket per day of the month, series id 'credits'
	// AIQuota is the org's stored settings (admin+ only; omitted otherwise).
	AIQuota *AIQuota `json:"aiQuota,omitempty"`
}

// MeAI is the `ai` field of GET /me (the current project's org).
type MeAI struct {
	Period    string   `json:"period"`
	Limit     *float64 `json:"limit"`
	Used      float64  `json:"used"`
	UserLimit *float64 `json:"userLimit"`
	UserUsed  float64  `json:"userUsed"`
}

// AIQuotaInput is PUT /orgs/{org}/ai-quota (monthlyCredits null = plan default, -1 = unlimited).
type AIQuotaInput struct {
	MonthlyCredits *int64   `json:"monthlyCredits"`
	Notify         []string `json:"notify"`
}

type AIQuota struct {
	OrgID          string   `json:"orgId"`
	MonthlyCredits *int64   `json:"monthlyCredits"`
	Notify         []string `json:"notify"`
}

// MemberAIQuotaInput is PUT /orgs/{org}/members/{user}/ai-quota (null = no member cap).
type MemberAIQuotaInput struct {
	MonthlyCredits *int64 `json:"monthlyCredits"`
}

type MemberAIQuota struct {
	OrgID          string `json:"orgId"`
	UserID         string `json:"userId"`
	MonthlyCredits *int64 `json:"monthlyCredits"`
}

// ---------------------------------------------------------------- hosts, containers, Kubernetes (§20)

// InfraStatus is ok | warning | critical | stale.
type InfraStatus string

const (
	InfraOK       InfraStatus = "ok"
	InfraWarning  InfraStatus = "warning"
	InfraCritical InfraStatus = "critical"
	InfraStale    InfraStatus = "stale"
)

type HostSpark struct {
	CPU    []float64 `json:"cpu"`
	Memory []float64 `json:"memory"`
}

type Host struct {
	ID          string      `json:"id"` // host.name
	HostID      string      `json:"hostId"`
	OS          string      `json:"os"`
	Arch        string      `json:"arch"`
	Cluster     string      `json:"cluster"`
	Node        string      `json:"node"`
	CPU         float64     `json:"cpu"`
	CPUCores    float64     `json:"cpuCores"`
	Memory      float64     `json:"memory"`
	MemoryBytes float64     `json:"memoryBytes"`
	Disk        float64     `json:"disk"`
	NetIn       float64     `json:"netIn"`
	NetOut      float64     `json:"netOut"`
	Load1       float64     `json:"load1"`
	UptimeS     float64     `json:"uptimeS"`
	Containers  int         `json:"containers"`
	LastSeen    int64       `json:"lastSeen"`
	Status      InfraStatus `json:"status"`
	Spark       HostSpark   `json:"spark"`
	GPUs        []HostGPU   `json:"gpus"` // latest minute per GPU (contracts §22); [] when none
}

// HostGPU is one GPU of a host (gpus_1m, latest minute): utilization / memory 0..1.
type HostGPU struct {
	Index        int     `json:"index"`
	Model        string  `json:"model"`
	UUID         string  `json:"uuid"`
	Utilization  float64 `json:"utilization"`
	Memory       float64 `json:"memory"`
	MemoryBytes  float64 `json:"memoryBytes"`
	TemperatureC float64 `json:"temperatureC"`
	PowerW       float64 `json:"powerW"`
}

type HostProcess struct {
	PID         uint32  `json:"pid"`
	Exe         string  `json:"exe"`
	Command     string  `json:"command"`
	ContainerID string  `json:"containerId"`
	CPU         float64 `json:"cpu"`
	MemoryBytes float64 `json:"memoryBytes"`
	Threads     float64 `json:"threads"`
	FDs         float64 `json:"fds"`
}

// HostSeries: cpu / memory / disk 0..1, network ids in / out (bytes/s).
type HostSeries struct {
	CPU     SeriesSet `json:"cpu"`
	Memory  SeriesSet `json:"memory"`
	Disk    SeriesSet `json:"disk"`
	Network SeriesSet `json:"network"`
	GPU     SeriesSet `json:"gpu"` // utilization 0..1 per GPU, ids gpu<index> (no series without GPUs)
}

type HostDetail struct {
	Host
	Series        HostSeries    `json:"series"`
	ContainerList []Container   `json:"containerList"`
	Processes     []HostProcess `json:"processes"`
	Services      []string      `json:"services"`
}

type Container struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Image       string      `json:"image"`
	Host        string      `json:"host"`
	Cluster     string      `json:"cluster"`
	Namespace   string      `json:"namespace"`
	Pod         string      `json:"pod"`
	PodUID      string      `json:"podUid"`
	Service     string      `json:"service"`
	CPU         float64     `json:"cpu"`
	CPULimit    float64     `json:"cpuLimit"`
	MemoryBytes float64     `json:"memoryBytes"`
	MemoryLimit float64     `json:"memoryLimit"`
	Restarts    float64     `json:"restarts"`
	NetIn       float64     `json:"netIn"`
	NetOut      float64     `json:"netOut"`
	Throttled   float64     `json:"throttled"`
	Status      InfraStatus `json:"status"`
}

type K8sCluster struct {
	Name              string      `json:"name"`
	Nodes             int         `json:"nodes"`
	NodesReady        int         `json:"nodesReady"`
	Pods              int         `json:"pods"`
	PodsRunning       int         `json:"podsRunning"`
	PodsFailing       int         `json:"podsFailing"`
	Namespaces        int         `json:"namespaces"`
	Workloads         int         `json:"workloads"`
	CPUUsed           float64     `json:"cpuUsed"`
	CPUAllocatable    float64     `json:"cpuAllocatable"`
	MemoryUsed        float64     `json:"memoryUsed"`
	MemoryAllocatable float64     `json:"memoryAllocatable"`
	WarningEvents     int64       `json:"warningEvents"`
	LastSeen          int64       `json:"lastSeen"`
	Status            InfraStatus `json:"status"`
}

type K8sNode struct {
	Name              string      `json:"name"`
	Ready             bool        `json:"ready"`
	CPU               float64     `json:"cpu"` // cores used
	CPUAllocatable    float64     `json:"cpuAllocatable"`
	MemoryBytes       float64     `json:"memoryBytes"`
	MemoryAllocatable float64     `json:"memoryAllocatable"`
	Pods              int         `json:"pods"`
	Host              string      `json:"host"`
	Status            InfraStatus `json:"status"`
}

type K8sNamespace struct {
	Name      string `json:"name"`
	Pods      int    `json:"pods"`
	Workloads int    `json:"workloads"`
	Failing   int    `json:"failing"`
}

// ResourceSeries: cpu (cores) / memory (bytes); ids used | requested | allocatable (cluster) or usage | request |
// limit (pod).
type ResourceSeries struct {
	CPU    SeriesSet `json:"cpu"`
	Memory SeriesSet `json:"memory"`
}

type K8sClusterDetail struct {
	K8sCluster
	NodeList      []K8sNode      `json:"nodeList"`
	NamespaceList []K8sNamespace `json:"namespaceList"`
	Series        ResourceSeries `json:"series"`
}

type K8sWorkload struct {
	Cluster     string      `json:"cluster"`
	Namespace   string      `json:"namespace"`
	Kind        string      `json:"kind"`
	Name        string      `json:"name"`
	Desired     float64     `json:"desired"`
	Ready       float64     `json:"ready"`
	Available   float64     `json:"available"`
	Updated     float64     `json:"updated"`
	Restarts    float64     `json:"restarts"` // increase in the range, over the workload's pods
	CPU         float64     `json:"cpu"`
	MemoryBytes float64     `json:"memoryBytes"`
	Service     string      `json:"service"`
	Status      InfraStatus `json:"status"`
}

type PodContainer struct {
	Name        string  `json:"name"`
	Image       string  `json:"image"`
	CPU         float64 `json:"cpu"`
	MemoryBytes float64 `json:"memoryBytes"`
	Restarts    float64 `json:"restarts"`
}

type Pod struct {
	UID             string         `json:"uid"`
	Name            string         `json:"name"`
	Namespace       string         `json:"namespace"`
	Cluster         string         `json:"cluster"`
	Node            string         `json:"node"`
	Phase           string         `json:"phase"`
	Reason          string         `json:"reason"`
	Ready           bool           `json:"ready"`
	Restarts        float64        `json:"restarts"`
	RestartsInRange float64        `json:"restartsInRange"`
	CPU             float64        `json:"cpu"`
	CPURequest      float64        `json:"cpuRequest"`
	CPULimit        float64        `json:"cpuLimit"`
	MemoryBytes     float64        `json:"memoryBytes"`
	MemoryRequest   float64        `json:"memoryRequest"`
	MemoryLimit     float64        `json:"memoryLimit"`
	WorkloadKind    string         `json:"workloadKind"`
	Workload        string         `json:"workload"`
	Service         string         `json:"service"`
	Status          InfraStatus    `json:"status"`
	Containers      []PodContainer `json:"containers"`
	LastSeen        int64          `json:"lastSeen"`
}

// PodSeries: cpu / memory ids usage | request | limit; restarts = restart increase per bucket (id restarts).
type PodSeries struct {
	CPU      SeriesSet `json:"cpu"`
	Memory   SeriesSet `json:"memory"`
	Restarts SeriesSet `json:"restarts"`
}

type PodDetail struct {
	Pod
	Series PodSeries  `json:"series"`
	Events []K8sEvent `json:"events"`
}

type K8sEvent struct {
	TS         int64  `json:"ts"`
	Type       string `json:"type"`
	Reason     string `json:"reason"`
	Message    string `json:"message"`
	ObjectKind string `json:"objectKind"`
	ObjectName string `json:"objectName"`
	Namespace  string `json:"namespace"`
	Cluster    string `json:"cluster"`
	Count      int64  `json:"count"`
}

type ServiceHost struct {
	ID     string  `json:"id"`
	CPU    float64 `json:"cpu"`
	Memory float64 `json:"memory"`
}

type ServiceFunction struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

// ServiceInfra is GET /services/{svc}/infra.
type ServiceInfra struct {
	Pods       []Pod             `json:"pods"`
	Hosts      []ServiceHost     `json:"hosts"`
	Containers []Container       `json:"containers"`
	Functions  []ServiceFunction `json:"functions"`
}

// ---------------------------------------------------------------- serverless (§21)

type ServerlessProvider struct {
	Provider    string  `json:"provider"`
	Functions   int     `json:"functions"`
	Invocations float64 `json:"invocations"`
	CostUSD     float64 `json:"costUsd"`
}

type ServerlessSummary struct {
	Functions   int                  `json:"functions"`
	Invocations float64              `json:"invocations"`
	Errors      float64              `json:"errors"`
	ErrorRate   float64              `json:"errorRate"`
	Throttles   float64              `json:"throttles"`
	ColdStarts  float64              `json:"coldStarts"`
	CostUSD     float64              `json:"costUsd"`
	ByProvider  []ServerlessProvider `json:"byProvider"`
}

type ServerlessFunction struct {
	ID             string    `json:"id"`
	Slug           string    `json:"slug"`
	Name           string    `json:"name"`
	Provider       string    `json:"provider"`
	App            string    `json:"app"`
	Region         string    `json:"region"`
	Runtime        string    `json:"runtime"`
	MemoryMb       *int64    `json:"memoryMb"`
	TimeoutS       *int64    `json:"timeoutS"`
	Arch           string    `json:"arch"`
	Triggers       []string  `json:"triggers"`
	Service        string    `json:"service"`
	Invocations    float64   `json:"invocations"`
	Errors         float64   `json:"errors"`
	ErrorRate      float64   `json:"errorRate"`
	Throttles      float64   `json:"throttles"`
	P95Ms          float64   `json:"p95Ms"`
	P99Ms          float64   `json:"p99Ms"`
	ColdStarts     float64   `json:"coldStarts"`
	ColdStartRate  float64   `json:"coldStartRate"`
	ConcurrencyMax float64   `json:"concurrencyMax"`
	CostUSD        float64   `json:"costUsd"`
	Status         string    `json:"status"` // ok | warning | critical | idle | missing
	Spark          []float64 `json:"spark"`  // invocations per bucket
	// ParentID is the id of the Azure function app row of a per-function row (meta.parent; contracts §22).
	ParentID string `json:"parentId,omitempty"`
	// ParentSlug is meta.parent (set even when the app row cannot be resolved); summary math only.
	ParentSlug string `json:"-"`
	// AppInsights is an Azure app row's meta.appInsights: connected | unavailable ('' = not probed yet).
	AppInsights string `json:"appInsights,omitempty"`
}

type FunctionError struct {
	TS      int64  `json:"ts"`
	TraceID string `json:"traceId"`
	Message string `json:"message"`
}

type IntegrationRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// FunctionSeries: invocations (ids invocations, errors, throttles), duration ms (avg, p95, p99), concurrency,
// coldStarts.
type FunctionSeries struct {
	Invocations SeriesSet `json:"invocations"`
	Duration    SeriesSet `json:"duration"`
	Concurrency SeriesSet `json:"concurrency"`
	ColdStarts  SeriesSet `json:"coldStarts"`
}

type ServerlessFunctionDetail struct {
	ServerlessFunction
	Series       FunctionSeries  `json:"series"`
	RecentErrors []FunctionError `json:"recentErrors"`
	Functions    []string        `json:"functions,omitempty"` // Azure: the app's functions
	// Children are the per-function rows of an Azure function app (App Insights; contracts §22); absent otherwise.
	Children    []ServerlessFunction `json:"children,omitempty"`
	Integration *IntegrationRef      `json:"integration"`
}
