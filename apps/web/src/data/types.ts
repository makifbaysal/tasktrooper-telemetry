export type TimeRange = '15m' | '30m' | '1h' | '6h' | '24h' | '7d';
export type Health = 'ok' | 'warning' | 'critical';
export type Severity = 'info' | 'warning' | 'critical';
export type Criticality = 'critical' | 'important' | 'low';
export type ServiceType = 'http' | 'worker' | 'consumer' | 'web' | 'mobile';

export interface Service {
  id: string;
  name: string;
  type: ServiceType;
  language: string;
  version: string;
  rpm: number;
  errorRate: number;
  p95: number;
  prevRpm: number;
  prevErrorRate: number;
  prevP95: number;
  status: Health;
  instances: number;
  lag?: number;
  /** 'ebpf' = seen through eBPF (OBI) spans, no SDK (ADR-019, contracts §20) */
  source?: 'sdk' | 'ebpf';
}

export interface Endpoint {
  id: string;
  serviceId: string;
  method: string;
  route: string;
  rpm: number;
  errorRate: number;
  p50: number;
  p95: number;
  p99: number;
  monitored: boolean;
  criticality: Criticality;
  /** consumers & workers: what the process reads instead of an HTTP route */
  sub?: Subscription;
}

/**
 * A consumer/worker process and the topic, queue or schedule it reads.
 * One topic can be read by several processes (one consumer group each), so the process is the row identity.
 */
export interface Subscription {
  process: string;
  source: 'kafka' | 'rabbitmq' | 'sqs' | 'cron';
  /** topic, queue name, or job name for cron */
  topic: string;
  group?: string;
  partitions?: number;
  schedule?: string;
  lag: number;
  retriesPerMin: number;
  dlqPerMin: number;
}

export interface RumPage {
  id: string;
  path: string;
  views: number;
  users: number;
  avgTime: number; // seconds
  lcp: number;
  inp: number;
  cls: number;
  errorRate: number;
  bounce: number;
}

export interface Session {
  id: string;
  user: string;
  identified: boolean;
  country: string;
  device: string;
  startedAt: number;
  duration: number;
  pages: number;
  errors: number;
  entry: string;
  exit: string;
  /** a session replay was recorded (ADR-021, contracts §23) */
  hasReplay?: boolean;
}

export interface LogLine {
  id: string;
  ts: number;
  level: 'debug' | 'info' | 'warn' | 'error';
  service: string;
  message: string;
  traceId?: string;
  /** platform log source, e.g. Vercel drains: build | static | external | edge | lambda | redirect | firewall */
  source?: string;
}

export type MonitorKind =
  | 'metric_threshold' | 'latency' | 'throughput_change' | 'anomaly' | 'log_match'
  | 'log_pattern_new' | 'error_new' | 'consumer_lag' | 'job_missed' | 'rum_vital' | 'kpi_burn' | 'composite';

export interface Monitor {
  id: string;
  name: string;
  kind: MonitorKind;
  target: string;
  condition: string;
  severity: Severity;
  state: 'ok' | 'pending' | 'firing' | 'muted';
  /** code = alert-as-code (`.trooper/monitors/*.yaml`, read-only in the UI) · suggestion = accepted code-intel suggestion */
  source: 'manual' | 'code' | 'template' | 'suggestion';
  /** source = code: `repo:path` of the YAML that defines it */
  codeRef?: string;
  lastChange: number;
  channels: string[];
  /** epoch ms; evaluated but not notified until then */
  mutedUntil?: number;
  /** consumer_lag monitors evaluated on broker data (Infrastructure → Message queues) */
  queue?: QueueRule;
}

/**
 * Lag alert on a consumer group. `topic` empty = every topic the group reads.
 * lag / partition_lag: messages · time_behind: seconds (lag ÷ produce rate) · growing: lag rose for `forMin`
 * · not_drained: hours since lag was last near zero (batch consumers) · rebalances: per hour.
 */
export interface QueueRule {
  clusterId: string;
  group: string;
  topic?: string;
  metric: 'lag' | 'time_behind' | 'partition_lag' | 'growing' | 'not_drained' | 'rebalances' | 'oldest_age';
  threshold: number;
  forMin: number;
  /** aws_sqs | gcp_pubsub | kafka | rabbitmq — set for oldest_age (seconds; SQS / Pub/Sub only) */
  messagingSystem?: string;
}

export interface Incident {
  id: string;
  monitorId: string;
  /** set when the incident comes from a KPI alert rule */
  kpiId?: string;
  title: string;
  severity: Severity;
  state: 'pending' | 'firing' | 'acknowledged' | 'resolved';
  startedAt: number;
  value: string;
  service: string;
  /** resolved from pending without ever firing — hidden from default lists */
  quiet?: boolean;
  ackedAt?: number;
  /** root cause stored by the engine when the incident opened (RcaResult shape) */
  rootCause?: unknown;
}

export interface Suggestion {
  id: string;
  title: string;
  kind: MonitorKind;
  target: string;
  condition: string;
  criticality: Criticality;
  confidence: number;
  rationale: string;
  codeRef: string;
  calibrated: boolean;
}

export interface LoadTestScenario {
  id: string;
  name: string;
  target: string;
  source: 'manual' | 'generated';
  steps: number;
  peakVus: number;
  duration: number;
  regions: string[];
  lastRun?: string;
}

export interface LoadTestRun {
  id: string;
  scenarioId: string;
  state: 'queued' | 'running' | 'passed' | 'failed' | 'aborted';
  startedAt: number;
  progress: number;
  rps: number;
  p95: number;
  errorRate: number;
  vus: number;
  thresholds: { name: string; passed: boolean }[];
}

export interface Repo {
  id: string;
  name: string;
  branch: string;
  sha: string;
  analyzedAt: number;
  services: string[];
  language: string;
  status: 'ready' | 'analyzing' | 'error';
}

export interface InventoryItem {
  id: string;
  kind: 'endpoint' | 'consumer' | 'cron' | 'view' | 'dependency';
  /** HTTP: "METHOD /route"; consumer/cron: the process name (several processes can read one topic). */
  key: string;
  /** Consumer: topic/queue it reads. Cron: the schedule. */
  source?: string;
  service: string;
  file: string;
  line: number;
  criticality: Criticality;
  observed: boolean;
  monitored: boolean;
}

/** Where request time goes. Slot = fixed categorical color. */
export type TimeCategory = 'app' | 'db' | 'cache' | 'http' | 'queue' | 'ai';

/** Outgoing dependency of a service (DB, cache, downstream HTTP, queue). */
export interface Dependency {
  id: string;
  serviceId: string;
  name: string;
  kind: Exclude<TimeCategory, 'app'>;
  avgMs: number;      // avg duration per call
  p95: number;
  errorRate: number;
  callsPerMin: number; // derived from endpoint fan-out × endpoint rpm
}

export interface EndpointCall {
  dep: Dependency;
  callsPerReq: number;
  msPerReq: number;   // callsPerReq × dep.avgMs
}

export interface TimeBreakdown {
  avgMs: number;                          // avg request duration
  parts: Record<TimeCategory, number>;    // ms per request by category (sums to avgMs)
}

/** Who sends traffic to a service. Resolved from trace context, RUM SDK, API key or user-agent. */
/** `topic` / `queue` / `schedule`: what a consumer or worker reads — they get messages, not requests. */
export type CallerKind = 'service' | 'web' | 'mobile' | 'partner' | 'webhook' | 'probe' | 'unknown' | 'topic' | 'queue' | 'schedule';

export interface Caller {
  id: string;
  name: string;
  kind: CallerKind;
  /** how we identified it, e.g. "API key · acme-erp" */
  via: string;
}

export interface CallerTraffic {
  caller: Caller;
  rpm: number;
  share: number;      // 0..1 of the target's incoming traffic
  errorRate: number;
  p95: number;
}

// ---------- KPIs (SLO-style objectives) ----------

/** What "good" means for a KPI's events. */
export type KpiSli = 'latency' | 'availability' | 'success_rate';

export interface KpiTarget {
  kind: 'page' | 'endpoint' | 'journey' | 'app_start';
  /** app (web/mobile) or backend service id */
  serviceId: string;
  /** page path, route template or journey name */
  ref: string;
  /** endpoint id when kind = endpoint */
  endpointId?: string;
}

export interface KpiAlertRule {
  id: string;
  kind: 'burn_rate' | 'sli_below';
  /** human readable, e.g. "budget burns 14.4× faster than allowed over 1h and 5m" */
  condition: string;
  severity: Severity;
  channels: string[];
  state: 'ok' | 'pending' | 'firing';
}

export interface Kpi {
  id: string;
  name: string;
  description: string;
  owner: string;
  target: KpiTarget;
  sli: KpiSli;
  /** human readable definition of a good event */
  goodWhen: string;
  threshold?: { metric: string; value: number };
  /** target share of good events, e.g. 0.95 */
  objective: number;
  window: '7d' | '28d';
  /** good-event ratio, last hour and over the whole window */
  current: number;
  windowGood: number;
  /** 0..1 of the error budget left in the window */
  budgetRemaining: number;
  burnRate1h: number;
  burnRate6h: number;
  status: Health;
  source: 'manual' | 'code' | 'template';
  alerts: KpiAlertRule[];
  /** when the current degradation started (if any) */
  degradedSince?: number;
  /** headline metric for latency KPIs: baseline vs now */
  metric?: { label: string; baseline: number; current: number; unit: 'ms' | 'ratio' };
}

// ---------- root cause analysis ----------

export type RcaNodeKind = 'page' | 'app_start' | 'endpoint' | 'db' | 'cache' | 'http' | 'queue' | 'ai' | 'external' | 'code';

/**
 * One node of the call tree behind a KPI (built from traces: page → API → service → dependency).
 * `baseline`/`current` are the node's contribution metric (ms per request, or failure ratio).
 */
export interface RcaNode {
  id: string;
  kind: RcaNodeKind;
  label: string;
  /** owning service (for endpoints & dependencies) */
  service?: string;
  /** in-app link to the node's page */
  link?: string;
  baseline: number;
  current: number;
  /** calls of this node per parent request, and whether they run in parallel with siblings */
  children?: { node: RcaNode; callsPerReq: number; parallel?: boolean }[];
  /** extra facts surfaced when this node is the root cause */
  evidence?: RcaEvidence[];
  /** facts about the node's own code (deploys, exceptions) — used when own code is the cause */
  ownEvidence?: RcaEvidence[];
}

export interface RcaEvidence {
  kind: 'deploy' | 'migration' | 'query' | 'resource' | 'log' | 'external';
  title: string;
  detail?: string;
  at?: number;
  link?: string;
}

export interface RcaHop {
  node: RcaNode;
  /** how much of the PARENT's change this hop explains (0..1) */
  explains: number;
  /** delta contributed to the parent, in the parent's unit */
  contribution: number;
  callsPerReq: number;
  /** siblings that also moved (> 15% of the parent's change) */
  alternatives: { node: RcaNode; explains: number }[];
}

export interface RcaResult {
  unit: 'ms' | 'ratio';
  path: RcaHop[];
  /** product of the per-hop shares — how sure the drill-down is */
  confidence: number;
  root: RcaNode;
  /** one-line answer, e.g. "… because recommendation-api's reco-db queries got 40× slower" */
  summary: string;
  /** engine (api): infra candidates — pod restarts / OOMKilled, saturated host (contracts §20 RCA) */
  infra?: RcaInfraCandidate[];
  /** engine (api): summary / confidence of the call-tree drill-down when infra candidates are also present */
  treeSummary?: string;
  treeConfidence?: number;
}

export interface RcaInfraCandidate {
  kind: 'pod' | 'host';
  entity: string;
  cluster?: string;
  node?: string;
  /** OOMKilled | CrashLoopBackOff | restarts | cpu_saturated … */
  reason: string;
  restarts?: number;
  value?: number;
  title: string;
  confidence: number;
}

// =====================================================================================================
// Hosts, containers, processes and Kubernetes (Faz 6, ADR-019, .ai/contracts.md §20) — owned by data/hosts.ts + data/k8s.ts
// =====================================================================================================

/** Status of an infra entity (host, node, pod, workload, cluster). `stale` = no data for > 5 min. */
export type InfraStatus = 'ok' | 'warning' | 'critical' | 'stale';

/** Api SeriesSet as returned by the infra endpoints. */
export interface InfraSeriesSet { timestamps: number[]; series: { id: string; label?: string; data: number[] }[] }

/** GET /infra/hosts — one host (VM or K8s node). cpu / memory / disk are 0..1, net bytes/s. */
export interface Host {
  id: string;
  hostId: string;
  os: string;
  arch: string;
  /** '' = not in Kubernetes */
  cluster: string;
  node: string;
  cpu: number;
  cpuCores: number;
  memory: number;
  memoryBytes: number;
  disk: number;
  netIn: number;
  netOut: number;
  load1: number;
  uptimeS: number;
  containers: number;
  lastSeen: number;
  status: InfraStatus;
  spark: { cpu: number[]; memory: number[] };
  /** GPUs (trooper-agent nvidia-smi, latest minute; contracts §22); [] when the host has none. */
  gpus: HostGPU[];
}

/** One GPU of a host: utilization / memory 0..1. */
export interface HostGPU {
  index: number;
  model: string;
  uuid: string;
  utilization: number;
  memory: number;
  memoryBytes: number;
  temperatureC: number;
  powerW: number;
}

/** GET /infra/containers. cpu in cores, memory in bytes, net bytes/s, throttled 0..1. */
export interface InfraContainer {
  id: string;
  name: string;
  image: string;
  host: string;
  cluster: string;
  namespace: string;
  pod: string;
  podUid: string;
  service: string;
  cpu: number;
  cpuLimit: number;
  memoryBytes: number;
  memoryLimit: number;
  restarts: number;
  netIn: number;
  netOut: number;
  throttled: number;
  status: InfraStatus;
}

export interface HostProcess { pid: number; exe: string; command: string; containerId: string; cpu: number; memoryBytes: number; threads: number; fds: number }

/** GET /infra/hosts/{host}: series are already fitted to the range buckets. */
export interface HostDetails extends Host {
  cpuSeries: number[];
  memorySeries: number[];
  diskSeries: number[];
  netInSeries: number[];
  netOutSeries: number[];
  /** Utilization (0..1) per GPU, id `gpu<index>`; empty without GPUs. */
  gpuSeries: { id: string; label: string; data: number[] }[];
  containerList: InfraContainer[];
  processes: HostProcess[];
  services: string[];
}

/** GET /infra/k8s/clusters. cpu in cores, memory in bytes. */
export interface K8sCluster {
  name: string;
  nodes: number;
  nodesReady: number;
  pods: number;
  podsRunning: number;
  podsFailing: number;
  namespaces: number;
  workloads: number;
  cpuUsed: number;
  cpuAllocatable: number;
  memoryUsed: number;
  memoryAllocatable: number;
  warningEvents: number;
  lastSeen: number;
  status: InfraStatus;
}

export interface K8sNode { name: string; ready: boolean; cpu: number; cpuAllocatable: number; memoryBytes: number; memoryAllocatable: number; pods: number; host: string; status?: InfraStatus }
export interface K8sNamespace { name: string; pods: number; workloads: number; failing: number }

/** GET /infra/k8s/clusters/{cluster}: `cpu` / `memory` series ids used | requested | allocatable. */
export interface K8sClusterDetails extends K8sCluster {
  nodeList: K8sNode[];
  namespaceList: K8sNamespace[];
  cpu: Record<'used' | 'requested' | 'allocatable', number[]>;
  memory: Record<'used' | 'requested' | 'allocatable', number[]>;
}

export type K8sWorkloadKind = 'Deployment' | 'StatefulSet' | 'DaemonSet' | 'Job' | 'CronJob';

export interface K8sWorkload {
  cluster: string;
  namespace: string;
  kind: K8sWorkloadKind | string;
  name: string;
  desired: number;
  ready: number;
  available: number;
  updated: number;
  /** restarts in the selected range */
  restarts: number;
  cpu: number;
  memoryBytes: number;
  service: string;
  status: InfraStatus;
}

export interface K8sPodContainer { name: string; image: string; cpu: number; memoryBytes: number; restarts: number }

export interface K8sPod {
  uid: string;
  name: string;
  namespace: string;
  cluster: string;
  node: string;
  /** Pending | Running | Succeeded | Failed | Unknown */
  phase: string;
  /** CrashLoopBackOff, OOMKilled, ImagePullBackOff … or '' */
  reason: string;
  ready: boolean;
  restarts: number;
  restartsInRange: number;
  cpu: number;
  cpuRequest: number;
  cpuLimit: number;
  memoryBytes: number;
  memoryRequest: number;
  memoryLimit: number;
  workloadKind: string;
  workload: string;
  service: string;
  status: InfraStatus;
  containers: K8sPodContainer[];
}

export interface K8sEvent { ts: number; type: string; reason: string; message: string; objectKind: string; objectName: string; namespace: string; cluster: string; count: number }

/** GET /infra/k8s/pods/{uid}: series ids usage | request | limit. */
export interface K8sPodDetails extends K8sPod {
  cpuSeries: Record<'usage' | 'request' | 'limit', number[]>;
  memorySeries: Record<'usage' | 'request' | 'limit', number[]>;
  restartSeries: number[];
  events: K8sEvent[];
}

/** GET /services/{svc}/infra */
export interface ServiceInfra {
  pods: K8sPod[];
  hosts: { id: string; cpu: number; memory: number }[];
  containers: InfraContainer[];
  functions: { id: string; name: string; provider: string }[];
}
