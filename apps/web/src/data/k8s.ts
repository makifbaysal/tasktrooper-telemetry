/**
 * Kubernetes clusters, nodes, workloads, pods and Warning events (ADR-019, .ai/contracts.md §20).
 * Collected by trooper-agent in `cluster` (K8s API) and `host` (kubelet) modes.
 * Data: /infra/k8s/*.
 */
import { RANGE_POINTS } from './apm';
import { hydrateList, qs, registerHydrator, remote, seg } from './remote';
import type {
  InfraContainer, InfraSeriesSet, InfraStatus, K8sCluster, K8sClusterDetails, K8sEvent, K8sNamespace, K8sNode, K8sPod, K8sPodContainer,
  K8sPodDetails, K8sWorkload, TimeRange,
} from './types';

export const GiB = 1024 ** 3;
export const MiB = 1024 ** 2;

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

// ---------------------------------------------------------------- api mapping helpers
const fit = (data: number[] | null | undefined, range: TimeRange) => {
  const n = RANGE_POINTS[range].points;
  const d = (data ?? []).map((v) => (Number.isFinite(v) ? v : 0));
  return d.length >= n ? d.slice(d.length - n) : [...Array<number>(n - d.length).fill(0), ...d];
};
export const pickInfraSeries = (raw: InfraSeriesSet | null | undefined, id: string, range: TimeRange) => fit(raw?.series?.find((x) => x.id === id)?.data, range);
const zeros = (range: TimeRange) => Array<number>(RANGE_POINTS[range].points).fill(0);
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

/** All clusters reporting through trooper-agent `cluster` mode. Hydrated in place. */
export const k8sClusters: K8sCluster[] = [];

export function k8sClusterDetails(name: string, range: TimeRange): K8sClusterDetails | undefined {
  const base = k8sClusters.find((c) => c.name === name);
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

export function k8sWorkloads(f: { cluster?: string; namespace?: string }, range: TimeRange): K8sWorkload[] {
  return remote(`/infra/k8s/workloads${qs({ cluster: f.cluster, namespace: f.namespace, range })}`, [] as K8sWorkload[], (raw: K8sWorkload[] | null) => (raw ?? []).map((w) => ({ ...w, status: asInfraStatus(w.status) })));
}

export interface PodFilter { cluster?: string; namespace?: string; workload?: string; node?: string; service?: string }
export function k8sPods(f: PodFilter, range: TimeRange): K8sPod[] {
  return remote(`/infra/k8s/pods${qs({ ...f, range })}`, [] as K8sPod[], (raw: K8sPod[] | null) => (raw ?? []).map(mapK8sPod));
}

export function k8sPodDetails(uid: string, range: TimeRange): K8sPodDetails | undefined {
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

export interface EventFilter { cluster?: string; namespace?: string; kind?: string; name?: string; limit?: number }
export function k8sEvents(f: EventFilter): K8sEvent[] {
  return remote(`/infra/k8s/events${qs({ cluster: f.cluster, namespace: f.namespace, kind: f.kind, name: f.name, limit: f.limit })}`, [] as K8sEvent[], (raw: K8sEvent[] | null) => raw ?? []);
}

registerHydrator(() => hydrateList(k8sClusters, '/infra/k8s/clusters', (raw: K8sCluster[] | null) => (raw ?? []).map(mapCluster)));
