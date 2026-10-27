/**
 * Kubernetes clusters, nodes, workloads, pods and Warning events (ADR-019, .ai/contracts.md §20).
 * Collected by trooper-agent in `cluster` (K8s API) and `host` (kubelet) modes.
 * Mock mode: two clusters (demo-eks, prod-gke) running the mock services; api mode: /infra/k8s/*.
 */
import { NOW, RANGE_POINTS, series } from './mock';
import { hydrateList, isApi, qs, registerHydrator, remote, seg } from './remote';
import type {
  InfraContainer, InfraSeriesSet, InfraStatus, K8sCluster, K8sClusterDetails, K8sEvent, K8sNamespace, K8sNode, K8sPod, K8sPodContainer,
  K8sPodDetails, K8sWorkload, TimeRange,
} from './types';

export const GiB = 1024 ** 3;
export const MiB = 1024 ** 2;

function h(s: string): number {
  let x = 2166136261;
  for (let i = 0; i < s.length; i++) x = Math.imul(x ^ s.charCodeAt(i), 16777619);
  return x >>> 0;
}
const ALPHA = 'bcdfghjklmnpqrstvwxz2456789';
const suffix = (key: string, n: number) => { let x = h(key); let s = ''; for (let i = 0; i < n; i++) { s += ALPHA[x % ALPHA.length]; x = Math.floor(x / ALPHA.length) || h(s + key); } return s; };

// ---------------------------------------------------------------- status rules (contracts §20)
const RANK: Record<InfraStatus, number> = { ok: 0, stale: 1, warning: 2, critical: 3 };
export const worstInfra = (xs: InfraStatus[]): InfraStatus => xs.reduce<InfraStatus>((a, b) => (RANK[b] > RANK[a] ? b : a), 'ok');
export const STALE_MS = 5 * 60_000;
export const POD_CRITICAL_REASONS = new Set(['CrashLoopBackOff', 'OOMKilled', 'ImagePullBackOff', 'ErrImagePull']);

export function utilStatus(values: number[], lastSeen: number, ready = true): InfraStatus {
  if (!ready || values.some((v) => v > 0.95)) return 'critical';
  if (Date.now() - lastSeen > STALE_MS) return 'stale';
  return values.some((v) => v > 0.85) ? 'warning' : 'ok';
}
export function podStatus(p: Pick<K8sPod, 'phase' | 'reason' | 'ready' | 'restartsInRange'>): InfraStatus {
  if (p.phase === 'Failed' || POD_CRITICAL_REASONS.has(p.reason) || (p.phase === 'Running' && !p.ready)) return 'critical';
  if (p.restartsInRange > 0 || p.phase === 'Pending') return 'warning';
  return 'ok';
}
export function workloadStatus(w: Pick<K8sWorkload, 'desired' | 'available'>): InfraStatus {
  if (w.desired > 0 && w.available === 0) return 'critical';
  return w.available < w.desired ? 'warning' : 'ok';
}

// ---------------------------------------------------------------- mock world
/** `extraCpu`: floor of the node's CPU use as a share of its cores (system daemons, noisy neighbours). */
interface NodeDef { name: string; cores: number; memGiB: number; extraCpu: number; ready: boolean }
interface WorkloadDef {
  ns: string; kind: K8sWorkload['kind']; name: string; service: string; replicas: number | 'node';
  cpuReq: number; cpuLim: number; memReq: number; memLim: number; cpuUse: number; memUse: number; image: string;
}
interface ClusterDef { name: string; nodes: NodeDef[]; workloads: WorkloadDef[] }

const W = (ns: string, kind: K8sWorkload['kind'], name: string, service: string, replicas: number | 'node', cpu: [number, number, number], memMi: [number, number, number], image: string): WorkloadDef =>
  ({ ns, kind, name, service, replicas, cpuReq: cpu[0], cpuLim: cpu[1], cpuUse: cpu[2], memReq: memMi[0] * MiB, memLim: memMi[1] * MiB, memUse: memMi[2] * MiB, image });

const EKS_NODE = (ip: string, extraCpu: number): NodeDef => ({ name: `ip-${ip}.eu-central-1.compute.internal`, cores: 8, memGiB: 30, extraCpu, ready: true });
const GKE_NODE = (id: string, extraCpu: number): NodeDef => ({ name: `gke-prod-eu-default-pool-${id}`, cores: 4, memGiB: 14, extraCpu, ready: true });

const DEFS: ClusterDef[] = [
  {
    name: 'demo-eks',
    nodes: [EKS_NODE('10-0-1-23', 0.42), EKS_NODE('10-0-2-41', 0.36), EKS_NODE('10-0-3-17', 0.88), EKS_NODE('10-0-1-88', 0.31)],
    workloads: [
      W('shop', 'Deployment', 'orders-api', 'orders-api', 4, [0.5, 1, 0.46], [512, 1024, 610], 'ghcr.io/acme/orders-api:v2.41.0'),
      W('shop', 'Deployment', 'payments-api', 'payments-api', 3, [0.5, 1, 0.38], [384, 768, 402], 'ghcr.io/acme/payments-api:v1.18.3'),
      W('shop', 'Deployment', 'catalog-api', 'catalog-api', 4, [0.5, 1, 0.52], [256, 512, 280], 'ghcr.io/acme/catalog-api:v3.2.0'),
      W('shop', 'Deployment', 'storefront-bff', 'storefront-bff', 3, [0.25, 0.5, 0.31], [256, 512, 300], 'ghcr.io/acme/storefront-bff:v1.9.4'),
      W('shop', 'Deployment', 'recommendation-api', 'recommendation-api', 3, [0.5, 1, 0.71], [768, 1024, 905], 'ghcr.io/acme/recommendation-api:v0.14.0'),
      W('async', 'Deployment', 'order-events-consumer', 'order-events', 3, [0.25, 0.5, 0.22], [256, 512, 260], 'ghcr.io/acme/order-events-consumer:v2.41.0'),
      W('async', 'Deployment', 'email-worker', 'email-worker', 2, [0.1, 0.5, 0.08], [2048, 2048, 410], 'ghcr.io/acme/email-worker:v0.9.12'),
      W('data', 'StatefulSet', 'reco-db', 'reco-db', 2, [2, 4, 3.1], [8192, 12288, 9100], 'postgres:16.2'),
      W('kube-system', 'Deployment', 'coredns', 'coredns', 2, [0.1, 0.2, 0.02], [70, 170, 38], 'coredns/coredns:1.11.3'),
      W('kube-system', 'DaemonSet', 'aws-node', 'aws-node', 'node', [0.025, 0.1, 0.01], [64, 128, 52], 'amazon-k8s-cni:v1.18.5'),
      W('trooper', 'DaemonSet', 'trooper-agent', 'trooper-agent', 'node', [0.05, 0.2, 0.03], [96, 256, 88], 'ghcr.io/tasktrooper/trooper-agent:0.6.0'),
      W('trooper', 'Deployment', 'trooper-agent-cluster', 'trooper-agent', 1, [0.05, 0.2, 0.02], [64, 128, 41], 'ghcr.io/tasktrooper/trooper-agent:0.6.0'),
    ],
  },
  {
    name: 'prod-gke',
    nodes: [GKE_NODE('7f3a-1c2d', 0.34), GKE_NODE('7f3a-9k8q', 0.27), GKE_NODE('7f3a-x4m2', 0.3)],
    workloads: [
      W('auth', 'Deployment', 'auth-api', 'auth-api', 3, [0.25, 0.5, 0.18], [256, 512, 230], 'ghcr.io/acme/auth-api:v5.0.2'),
      W('shop', 'Deployment', 'catalog-api', 'catalog-api', 3, [0.5, 1, 0.41], [256, 512, 262], 'ghcr.io/acme/catalog-api:v3.2.0'),
      W('ingress-nginx', 'Deployment', 'ingress-nginx-controller', 'ingress-nginx', 2, [0.1, 0.5, 0.12], [90, 256, 130], 'registry.k8s.io/ingress-nginx/controller:v1.11.2'),
      W('kube-system', 'DaemonSet', 'kube-proxy', 'kube-proxy', 'node', [0.1, 0.1, 0.01], [32, 64, 24], 'registry.k8s.io/kube-proxy:v1.31.1'),
      W('trooper', 'DaemonSet', 'trooper-agent', 'trooper-agent', 'node', [0.05, 0.2, 0.03], [96, 256, 80], 'ghcr.io/tasktrooper/trooper-agent:0.6.0'),
      W('trooper', 'DaemonSet', 'trooper-obi', 'trooper-obi', 'node', [0.1, 0.5, 0.06], [128, 512, 150], 'otel/ebpf-instrument:v0.2.0'),
      W('trooper', 'Deployment', 'trooper-agent-cluster', 'trooper-agent', 1, [0.05, 0.2, 0.02], [64, 128, 39], 'ghcr.io/tasktrooper/trooper-agent:0.6.0'),
    ],
  },
];

/** Hand-placed problems: one CrashLoopBackOff pod killed by OOM, one unschedulable pod, a restarted sibling. */
const SPECIAL: Record<string, Partial<K8sPod>> = {
  'demo-eks/recommendation-api#0': { reason: 'CrashLoopBackOff', ready: false, restarts: 14, restartsInRange: 6, memoryBytes: 1018 * MiB, cpu: 0.04 },
  'demo-eks/recommendation-api#1': { restarts: 2, restartsInRange: 1, memoryBytes: 940 * MiB },
  'demo-eks/email-worker#1': { phase: 'Pending', ready: false, node: '', cpu: 0, memoryBytes: 0 },
};

interface MockPod extends K8sPod { image: string }
interface World { clusters: K8sCluster[]; nodes: Record<string, K8sNode[]>; workloads: K8sWorkload[]; pods: MockPod[]; events: K8sEvent[] }

function buildWorld(): World {
  const pods: MockPod[] = [];
  const workloads: K8sWorkload[] = [];
  const nodes: Record<string, K8sNode[]> = {};
  for (const c of DEFS) {
    for (const w of c.workloads) {
      const n = w.replicas === 'node' ? c.nodes.length : w.replicas;
      const rs = suffix(`${c.name}/${w.name}/rs`, 9);
      const wPods: MockPod[] = [];
      for (let i = 0; i < n; i++) {
        const name = w.kind === 'StatefulSet' ? `${w.name}-${i}` : w.kind === 'DaemonSet' ? `${w.name}-${suffix(`${c.name}/${w.name}/${i}`, 5)}` : `${w.name}-${rs}-${suffix(`${c.name}/${w.name}/${i}`, 5)}`;
        const node = w.replicas === 'node' ? c.nodes[i].name : c.nodes[(h(`${c.name}/${w.name}`) + i) % c.nodes.length].name;
        const jitter = 0.8 + (h(name) % 40) / 100;
        const sp = SPECIAL[`${c.name}/${w.name}#${i}`] ?? {};
        const base = {
          uid: `${suffix(name + 'uid', 8)}-${suffix(name + 'u2', 4)}-${suffix(name + 'u3', 4)}-${suffix(name + 'u4', 12)}`,
          name, namespace: w.ns, cluster: c.name, node, phase: 'Running', reason: '', ready: true,
          restarts: h(name + 'r') % 7 === 0 ? 1 : 0, restartsInRange: 0,
          cpu: +(w.cpuUse * jitter).toFixed(3), cpuRequest: w.cpuReq, cpuLimit: w.cpuLim,
          memoryBytes: Math.round(w.memUse * jitter), memoryRequest: w.memReq, memoryLimit: w.memLim,
          workloadKind: w.kind, workload: w.name, service: w.service, image: w.image,
          ...sp,
        };
        const pod: MockPod = { ...base, status: 'ok', containers: [] };
        pod.status = podStatus(pod);
        pod.containers = [{ name: w.name.replace(/-controller$/, ''), image: w.image, cpu: pod.cpu, memoryBytes: pod.memoryBytes, restarts: pod.restarts }];
        wPods.push(pod);
      }
      pods.push(...wPods);
      const ready = wPods.filter((p) => p.ready).length;
      const wl: K8sWorkload = {
        cluster: c.name, namespace: w.ns, kind: w.kind, name: w.name, desired: n, ready, available: ready, updated: n,
        restarts: wPods.reduce((a, p) => a + p.restartsInRange, 0), cpu: wPods.reduce((a, p) => a + p.cpu, 0),
        memoryBytes: wPods.reduce((a, p) => a + p.memoryBytes, 0), service: w.service, status: 'ok',
      };
      wl.status = workloadStatus(wl);
      workloads.push(wl);
    }
    nodes[c.name] = c.nodes.map((d) => {
      const on = pods.filter((p) => p.cluster === c.name && p.node === d.name);
      const nd: K8sNode = {
        name: d.name, ready: d.ready, cpu: +Math.max(on.reduce((a, p) => a + p.cpu, 0) + 0.15, d.extraCpu * d.cores).toFixed(2), cpuAllocatable: d.cores - 0.1,
        memoryBytes: on.reduce((a, p) => a + p.memoryBytes, 0) + 1.1 * GiB, memoryAllocatable: d.memGiB * GiB, pods: on.length, host: d.name,
      };
      nd.status = utilStatus([nd.cpu / nd.cpuAllocatable, nd.memoryBytes / nd.memoryAllocatable], NOW, nd.ready);
      return nd;
    });
  }

  const ev = (cluster: string, minAgo: number, reason: string, objectKind: string, objectName: string, namespace: string, count: number, message: string): K8sEvent =>
    ({ ts: NOW - minAgo * 60_000, type: 'Warning', reason, message, objectKind, objectName, namespace, cluster, count });
  const crash = pods.find((p) => p.reason === 'CrashLoopBackOff')!;
  const sibling = pods.find((p) => p.workload === 'recommendation-api' && p.restartsInRange === 1)!;
  const pending = pods.find((p) => p.phase === 'Pending')!;
  const bff = pods.find((p) => p.workload === 'storefront-bff')!;
  const ingress = pods.find((p) => p.workload === 'ingress-nginx-controller')!;
  const events: K8sEvent[] = [
    ev('demo-eks', 1, 'BackOff', 'Pod', crash.name, 'shop', 23, `Back-off restarting failed container recommendation-api in pod ${crash.name}`),
    ev('demo-eks', 3, 'OOMKilling', 'Node', crash.node, '', 6, 'Memory cgroup out of memory: Killed process 1 (python3.12) total-vm:1912044kB, anon-rss:1042180kB — container recommendation-api was OOMKilled (limit 1Gi)'),
    ev('demo-eks', 4, 'Unhealthy', 'Pod', crash.name, 'shop', 9, 'Readiness probe failed: Get "http://10.0.3.112:8000/healthz": context deadline exceeded'),
    ev('demo-eks', 6, 'FailedScheduling', 'Pod', pending.name, 'async', 12, '0/4 nodes are available: 4 Insufficient memory. preemption: 0/4 nodes are available: 4 No preemption victims found for incoming pod.'),
    ev('demo-eks', 11, 'Unhealthy', 'Pod', bff.name, 'shop', 4, 'Liveness probe failed: HTTP probe failed with statuscode: 503'),
    ev('demo-eks', 19, 'OOMKilling', 'Node', sibling.node, '', 1, `Memory cgroup out of memory: container recommendation-api in pod ${sibling.name} was OOMKilled (limit 1Gi)`),
    ev('prod-gke', 27, 'Unhealthy', 'Pod', ingress.name, 'ingress-nginx', 2, 'Readiness probe failed: HTTP probe failed with statuscode: 500'),
    ev('prod-gke', 48, 'FailedMount', 'Pod', ingress.name, 'ingress-nginx', 1, 'MountVolume.SetUp failed for volume "webhook-cert": secret "ingress-nginx-admission" not found'),
  ];

  const clusters: K8sCluster[] = DEFS.map((c) => {
    const ns = nodes[c.name];
    const cp = pods.filter((p) => p.cluster === c.name);
    const cl: K8sCluster = {
      name: c.name, nodes: ns.length, nodesReady: ns.filter((n) => n.ready).length, pods: cp.length,
      podsRunning: cp.filter((p) => p.phase === 'Running').length, podsFailing: cp.filter((p) => p.status === 'critical').length,
      namespaces: new Set(cp.map((p) => p.namespace)).size, workloads: workloads.filter((w) => w.cluster === c.name).length,
      cpuUsed: +ns.reduce((a, n) => a + n.cpu, 0).toFixed(2), cpuAllocatable: ns.reduce((a, n) => a + n.cpuAllocatable, 0),
      memoryUsed: ns.reduce((a, n) => a + n.memoryBytes, 0), memoryAllocatable: ns.reduce((a, n) => a + n.memoryAllocatable, 0),
      warningEvents: events.filter((e) => e.cluster === c.name).reduce((a, e) => a + e.count, 0), lastSeen: NOW, status: 'ok',
    };
    cl.status = worstInfra([...ns.map((n) => n.status ?? 'ok'), ...cp.map((p) => p.status)]);
    return cl;
  });
  return { clusters, nodes, workloads, pods, events };
}

const world: World = isApi ? { clusters: [], nodes: {}, workloads: [], pods: [], events: [] } : buildWorld();

/** Mock containers of the pods (one per pod) — data/hosts.ts adds the non-K8s Docker containers. */
export function mockPodContainers(): InfraContainer[] {
  return world.pods.filter((p) => p.node).map((p) => {
    const c = p.containers[0];
    return {
      id: suffix(p.uid + 'cid', 12) + suffix(p.uid + 'cid2', 12), name: c.name, image: c.image, host: p.node, cluster: p.cluster, namespace: p.namespace,
      pod: p.name, podUid: p.uid, service: p.service, cpu: p.cpu, cpuLimit: p.cpuLimit, memoryBytes: p.memoryBytes, memoryLimit: p.memoryLimit,
      restarts: p.restarts, netIn: Math.round(p.cpu * 380_000), netOut: Math.round(p.cpu * 520_000),
      throttled: p.cpu / p.cpuLimit > 0.65 ? +((p.cpu / p.cpuLimit - 0.6) * 0.5).toFixed(3) : 0, status: p.status === 'critical' ? 'critical' : 'ok',
    };
  });
}
/** Mock node list per cluster (data/hosts.ts turns nodes into hosts). */
export const mockK8sNodes = () => world.nodes;
export const mockK8sPods = (): K8sPod[] => world.pods;

// ---------------------------------------------------------------- api mapping helpers
const fit = (data: number[] | null | undefined, range: TimeRange) => {
  const n = RANGE_POINTS[range].points;
  const d = (data ?? []).map((v) => (Number.isFinite(v) ? v : 0));
  return d.length >= n ? d.slice(d.length - n) : [...Array<number>(n - d.length).fill(0), ...d];
};
export const pickInfraSeries = (raw: InfraSeriesSet | null | undefined, id: string, range: TimeRange) => fit(raw?.series?.find((x) => x.id === id)?.data, range);
const zeros = (range: TimeRange) => Array<number>(RANGE_POINTS[range].points).fill(0);
const flat = (v: number, range: TimeRange) => Array<number>(RANGE_POINTS[range].points).fill(v);
const STATUSES = new Set<InfraStatus>(['ok', 'warning', 'critical', 'stale']);
export const asInfraStatus = (s: unknown): InfraStatus => (STATUSES.has(s as InfraStatus) ? (s as InfraStatus) : 'ok');
const n0 = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);

export function mapK8sPod(r: Partial<K8sPod> & { uid: string }): K8sPod {
  return {
    uid: r.uid, name: r.name ?? r.uid, namespace: r.namespace ?? '', cluster: r.cluster ?? '', node: r.node ?? '', phase: r.phase ?? 'Unknown',
    reason: r.reason ?? '', ready: !!r.ready, restarts: n0(r.restarts), restartsInRange: n0(r.restartsInRange), cpu: n0(r.cpu), cpuRequest: n0(r.cpuRequest),
    cpuLimit: n0(r.cpuLimit), memoryBytes: n0(r.memoryBytes), memoryRequest: n0(r.memoryRequest), memoryLimit: n0(r.memoryLimit),
    workloadKind: r.workloadKind ?? '', workload: r.workload ?? '', service: r.service ?? '', status: asInfraStatus(r.status),
    containers: (r.containers ?? []).map((c: K8sPodContainer) => ({ ...c, cpu: n0(c.cpu), memoryBytes: n0(c.memoryBytes), restarts: n0(c.restarts) })),
  };
}
export function mapInfraContainer(r: Partial<InfraContainer> & { id: string }): InfraContainer {
  return {
    id: r.id, name: r.name ?? r.id.slice(0, 12), image: r.image ?? '', host: r.host ?? '', cluster: r.cluster ?? '', namespace: r.namespace ?? '', pod: r.pod ?? '',
    podUid: r.podUid ?? '', service: r.service ?? '', cpu: n0(r.cpu), cpuLimit: n0(r.cpuLimit), memoryBytes: n0(r.memoryBytes), memoryLimit: n0(r.memoryLimit),
    restarts: n0(r.restarts), netIn: n0(r.netIn), netOut: n0(r.netOut), throttled: n0(r.throttled), status: asInfraStatus(r.status),
  };
}
const mapCluster = (r: K8sCluster): K8sCluster => ({ ...r, status: asInfraStatus(r.status) });

// ---------------------------------------------------------------- public api (sync, ADR-013)

/** All clusters reporting through trooper-agent `cluster` mode. Hydrated in place in api mode. */
export const k8sClusters: K8sCluster[] = world.clusters;

export function k8sClusterDetails(name: string, range: TimeRange): K8sClusterDetails | undefined {
  const base = k8sClusters.find((c) => c.name === name);
  if (isApi) {
    type Raw = K8sCluster & { nodeList?: K8sNode[] | null; namespaceList?: K8sNamespace[] | null; series?: { cpu?: InfraSeriesSet; memory?: InfraSeriesSet } | null };
    const fallback = base ? { ...base, nodeList: [], namespaceList: [], cpu: { used: zeros(range), requested: zeros(range), allocatable: zeros(range) }, memory: { used: zeros(range), requested: zeros(range), allocatable: zeros(range) } } : undefined;
    return remote<K8sClusterDetails | undefined>(`/infra/k8s/clusters/${seg(name)}${qs({ range })}`, fallback, (r: Raw) => {
      const three = (s: InfraSeriesSet | undefined) => ({ used: pickInfraSeries(s, 'used', range), requested: pickInfraSeries(s, 'requested', range), allocatable: pickInfraSeries(s, 'allocatable', range) });
      const { nodeList, namespaceList, series, ...row } = r;
      return {
        ...mapCluster(row),
        nodeList: (nodeList ?? []).map((n) => ({ ...n, status: asInfraStatus(n.status) })),
        namespaceList: namespaceList ?? [], cpu: three(series?.cpu), memory: three(series?.memory),
      };
    });
  }
  if (!base) return undefined;
  const cp = world.pods.filter((p) => p.cluster === name);
  const nodeList = world.nodes[name] ?? [];
  const namespaceList: K8sNamespace[] = [...new Set(cp.map((p) => p.namespace))].sort().map((ns) => ({
    name: ns, pods: cp.filter((p) => p.namespace === ns).length,
    workloads: world.workloads.filter((w) => w.cluster === name && w.namespace === ns).length,
    failing: cp.filter((p) => p.namespace === ns && p.status === 'critical').length,
  }));
  const cpuReq = cp.reduce((a, p) => a + (p.node ? p.cpuRequest : 0), 0);
  const memReq = cp.reduce((a, p) => a + (p.node ? p.memoryRequest : 0), 0);
  return {
    ...base, nodeList, namespaceList,
    cpu: { used: series(`k8s-cpu-${name}`, range, { base: base.cpuUsed, noise: 0.08, daily: 0.2 }), requested: flat(cpuReq, range), allocatable: flat(base.cpuAllocatable, range) },
    memory: { used: series(`k8s-mem-${name}`, range, { base: base.memoryUsed, noise: 0.03, daily: 0.05 }), requested: flat(memReq, range), allocatable: flat(base.memoryAllocatable, range) },
  };
}

export function k8sWorkloads(f: { cluster?: string; namespace?: string }, range: TimeRange): K8sWorkload[] {
  if (isApi) return remote(`/infra/k8s/workloads${qs({ cluster: f.cluster, namespace: f.namespace, range })}`, [] as K8sWorkload[], (raw: K8sWorkload[] | null) => (raw ?? []).map((w) => ({ ...w, status: asInfraStatus(w.status) })));
  return world.workloads.filter((w) => (!f.cluster || w.cluster === f.cluster) && (!f.namespace || w.namespace === f.namespace));
}

export interface PodFilter { cluster?: string; namespace?: string; workload?: string; node?: string; service?: string }
export function k8sPods(f: PodFilter, range: TimeRange): K8sPod[] {
  if (isApi) return remote(`/infra/k8s/pods${qs({ ...f, range })}`, [] as K8sPod[], (raw: K8sPod[] | null) => (raw ?? []).map(mapK8sPod));
  return world.pods.filter((p) => (!f.cluster || p.cluster === f.cluster) && (!f.namespace || p.namespace === f.namespace) && (!f.workload || p.workload === f.workload)
    && (!f.node || p.node === f.node) && (!f.service || p.service === f.service));
}

export function k8sPodDetails(uid: string, range: TimeRange): K8sPodDetails | undefined {
  if (isApi) {
    type Raw = K8sPod & { series?: { cpu?: InfraSeriesSet; memory?: InfraSeriesSet; restarts?: InfraSeriesSet } | null; events?: K8sEvent[] | null };
    return remote<K8sPodDetails | undefined>(`/infra/k8s/pods/${seg(uid)}${qs({ range })}`, undefined, (r: Raw) => {
      const three = (s: InfraSeriesSet | undefined) => ({ usage: pickInfraSeries(s, 'usage', range), request: pickInfraSeries(s, 'request', range), limit: pickInfraSeries(s, 'limit', range) });
      const { series, events, ...row } = r;
      return {
        ...mapK8sPod(row), cpuSeries: three(series?.cpu), memorySeries: three(series?.memory),
        // restarts: increase per bucket
        restartSeries: fit(series?.restarts?.series?.[0]?.data, range), events: events ?? [],
      };
    });
  }
  const p = world.pods.find((x) => x.uid === uid);
  if (!p) return undefined;
  const crash = p.reason === 'CrashLoopBackOff';
  const n = RANGE_POINTS[range].points;
  // a pod that keeps getting OOMKilled: memory climbs to the limit, drops to zero on each restart
  const mem = crash
    ? Array.from({ length: n }, (_, i) => (i < n * 0.6 ? p.memoryLimit * 0.82 : ((i % 5) / 4) * p.memoryLimit * 0.995))
    : series(`pod-mem-${uid}`, range, { base: p.memoryBytes, noise: 0.03, daily: 0.02 });
  const restarts = Array.from({ length: n }, (_, i) => (p.restartsInRange > 0 && i >= n * 0.6 && (crash ? i % 5 === 4 : i === n - 4) ? 1 : 0));
  return {
    ...p,
    cpuSeries: { usage: p.phase === 'Pending' ? zeros(range) : series(`pod-cpu-${uid}`, range, { base: Math.max(p.cpu, 0.01), noise: 0.15, daily: 0.2 }), request: flat(p.cpuRequest, range), limit: flat(p.cpuLimit, range) },
    memorySeries: { usage: p.phase === 'Pending' ? zeros(range) : mem, request: flat(p.memoryRequest, range), limit: flat(p.memoryLimit, range) },
    restartSeries: restarts,
    events: k8sEvents({ cluster: p.cluster, name: p.name }).concat(k8sEvents({ cluster: p.cluster, kind: 'Node', name: p.node }).filter((e) => e.message.includes(p.name) || e.message.includes(p.workload))),
  };
}

export interface EventFilter { cluster?: string; namespace?: string; kind?: string; name?: string; limit?: number }
export function k8sEvents(f: EventFilter): K8sEvent[] {
  if (isApi) return remote(`/infra/k8s/events${qs({ cluster: f.cluster, namespace: f.namespace, kind: f.kind, name: f.name, limit: f.limit })}`, [] as K8sEvent[], (raw: K8sEvent[] | null) => raw ?? []);
  return world.events
    .filter((e) => (!f.cluster || e.cluster === f.cluster) && (!f.namespace || e.namespace === f.namespace) && (!f.kind || e.objectKind === f.kind) && (!f.name || e.objectName === f.name))
    .sort((a, b) => b.ts - a.ts)
    .slice(0, f.limit ?? 200);
}

if (isApi) {
  registerHydrator(() => hydrateList(k8sClusters, '/infra/k8s/clusters', (raw: K8sCluster[] | null) => (raw ?? []).map(mapCluster)));
}
