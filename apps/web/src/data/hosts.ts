/**
 * Hosts, containers and processes (ADR-019, .ai/contracts.md §20) — trooper-agent `host` mode.
 * A host is a VM or a Kubernetes node; containers come from kubelet (K8s) or the Docker socket (VMs).
 * Mock mode: the nodes of data/k8s.ts plus a few VMs outside Kubernetes; api mode: /infra/hosts, /infra/containers.
 */
import { NOW, RANGE_POINTS, series } from './mock';
import { hydrateList, isApi, qs, registerHydrator, remote, remoteStatus, seg } from './remote';
import { GiB, MiB, asInfraStatus, k8sClusters, k8sPods, mapInfraContainer, mapK8sPod, mockK8sNodes, mockPodContainers, pickInfraSeries, utilStatus } from './k8s';
import type { Host, HostDetails, HostGPU, HostProcess, InfraContainer, InfraSeriesSet, ServiceInfra, TimeRange } from './types';

function h(s: string): number {
  let x = 2166136261;
  for (let i = 0; i < s.length; i++) x = Math.imul(x ^ s.charCodeAt(i), 16777619);
  return x >>> 0;
}
const frac = (key: string, lo: number, hi: number) => lo + ((h(key) % 1000) / 1000) * (hi - lo);

// ---------------------------------------------------------------- mock world
interface VmDef {
  id: string; os: string; cores: number; memGiB: number; cpu: number; memory: number; disk: number; lastSeenAgo: number; containers: [string, string, string, number, number][];
  /** GPUs: [model, utilization, memory 0..1, memory total GiB, temperature °C, power W] */
  gpus?: [string, number, number, number, number, number][];
}
/** VMs outside Kubernetes: Docker containers [name, image, service, cpu cores, memory MiB]. */
const VMS: VmDef[] = [
  { id: 'backup-vm-01', os: 'linux · Ubuntu 24.04', cores: 4, memGiB: 16, cpu: 0.18, memory: 0.41, disk: 0.72, lastSeenAgo: 0,
    containers: [['pg-backup', 'ghcr.io/acme/pg-backup:1.4.0', 'pg-backup', 0.4, 900], ['node-exporter', 'prom/node-exporter:v1.8.2', 'node-exporter', 0.02, 24]] },
  { id: 'legacy-billing-01', os: 'linux · Debian 12', cores: 2, memGiB: 8, cpu: 0.89, memory: 0.78, disk: 0.91, lastSeenAgo: 0,
    containers: [['billing-legacy', 'acme/billing-legacy:2019.11', 'billing-legacy', 1.6, 4600], ['nginx', 'nginx:1.27-alpine', 'billing-legacy', 0.08, 40]] },
  { id: 'bastion-01', os: 'linux · Amazon Linux 2023', cores: 2, memGiB: 4, cpu: 0.03, memory: 0.22, disk: 0.34, lastSeenAgo: 12, containers: [] },
  { id: 'ml-train-01', os: 'linux · Ubuntu 22.04', cores: 32, memGiB: 256, cpu: 0.46, memory: 0.58, disk: 0.49, lastSeenAgo: 0,
    containers: [['recommendation-trainer', 'ghcr.io/acme/recommendation-trainer:0.9.3', 'recommendation', 11.5, 98_000]],
    gpus: [['NVIDIA A100-SXM4-80GB', 0.87, 0.71, 80, 71, 342], ['NVIDIA A100-SXM4-80GB', 0.34, 0.22, 80, 54, 141]] },
];

interface MockWorld { hosts: Host[]; containers: InfraContainer[] }

function buildWorld(): MockWorld {
  const containers: InfraContainer[] = mockPodContainers();
  const hosts: Host[] = [];
  for (const [cluster, nodes] of Object.entries(mockK8sNodes())) {
    for (const n of nodes) {
      const mine = containers.filter((c) => c.host === n.name);
      const memTotal = n.memoryAllocatable + 2 * GiB;
      const cores = Math.round(n.cpuAllocatable + 0.1);
      const host: Host = {
        id: n.name, hostId: (h(n.name).toString(16) + h(n.name + 'm').toString(16)).padEnd(32, '0'),
        os: cluster.endsWith('eks') ? 'linux · Amazon Linux 2023' : 'linux · Container-Optimized OS', arch: cluster.endsWith('eks') ? 'arm64' : 'amd64',
        cluster, node: n.name, cpu: Math.min(1, n.cpu / cores), cpuCores: cores, memory: n.memoryBytes / memTotal, memoryBytes: memTotal,
        disk: frac(n.name + 'disk', 0.28, 0.62), netIn: mine.reduce((a, c) => a + c.netIn, 0), netOut: mine.reduce((a, c) => a + c.netOut, 0),
        load1: +((n.cpu) * 1.1).toFixed(2), uptimeS: Math.round(frac(n.name + 'up', 3, 40) * 86400), containers: mine.length, lastSeen: NOW,
        status: 'ok', spark: { cpu: [], memory: [] }, gpus: [],
      };
      host.status = utilStatus([host.cpu, host.memory, host.disk], host.lastSeen, n.ready);
      hosts.push(host);
    }
  }
  for (const v of VMS) {
    const mine: InfraContainer[] = v.containers.map(([name, image, service, cpu, memMi]) => ({
      id: (h(v.id + name).toString(16) + h(name + v.id).toString(16)).padEnd(24, '0'), name, image, host: v.id, cluster: '', namespace: '', pod: '', podUid: '', service,
      cpu, cpuLimit: 0, memoryBytes: memMi * MiB, memoryLimit: 0, restarts: 0, netIn: Math.round(cpu * 210_000), netOut: Math.round(cpu * 90_000), throttled: 0, status: 'ok',
    }));
    containers.push(...mine);
    const lastSeen = NOW - v.lastSeenAgo * 60_000;
    const host: Host = {
      id: v.id, hostId: (h(v.id).toString(16) + h(v.id + 'm').toString(16)).padEnd(32, '0'), os: v.os, arch: 'amd64', cluster: '', node: '',
      cpu: v.cpu, cpuCores: v.cores, memory: v.memory, memoryBytes: v.memGiB * GiB, disk: v.disk,
      netIn: mine.reduce((a, c) => a + c.netIn, 0) + 12_000, netOut: mine.reduce((a, c) => a + c.netOut, 0) + 8_000, load1: +(v.cpu * v.cores * 1.2).toFixed(2),
      uptimeS: Math.round(frac(v.id + 'up', 20, 400) * 86400), containers: mine.length, lastSeen, status: 'ok', spark: { cpu: [], memory: [] },
      gpus: (v.gpus ?? []).map(([model, utilization, memory, memGiB, temperatureC, powerW], index): HostGPU => ({
        index, model, uuid: `GPU-${h(v.id + index).toString(16).padStart(8, '0')}-${h(model + index).toString(16).slice(0, 4)}`,
        utilization, memory, memoryBytes: memory * memGiB * GiB, temperatureC, powerW,
      })),
    };
    host.status = utilStatus([host.cpu, host.memory, host.disk], lastSeen);
    hosts.push(host);
  }
  for (const x of hosts) x.spark = { cpu: series(`host-cpu-${x.id}`, '1h', { base: x.cpu, noise: 0.12, daily: 0.15 }).slice(-24).map((v) => Math.min(1, v)), memory: series(`host-mem-${x.id}`, '1h', { base: x.memory, noise: 0.02, daily: 0.03 }).slice(-24) };
  return { hosts, containers };
}

const world: MockWorld = isApi ? { hosts: [], containers: [] } : buildWorld();

const SYSTEM_PROCS: Record<string, [string, string, number, number][]> = {
  node: [['kubelet', '/usr/bin/kubelet', 0.12, 140], ['containerd', '/usr/bin/containerd', 0.06, 95], ['trooper-agent', '/trooper-agent', 0.03, 88], ['systemd-journald', '/usr/lib/systemd/systemd-journald', 0.01, 60]],
  vm: [['dockerd', '/usr/bin/dockerd', 0.03, 110], ['containerd', '/usr/bin/containerd', 0.02, 60], ['trooper-agent', '/usr/local/bin/trooper-agent', 0.02, 70], ['sshd', '/usr/sbin/sshd', 0, 8]],
};
/** Main process of a container image (only the executable — arguments are never collected). */
function exeOf(c: InfraContainer): [string, string] {
  if (c.image.startsWith('postgres')) return ['postgres', '/usr/lib/postgresql/16/bin/postgres'];
  if (c.image.includes('nginx')) return ['nginx', 'nginx'];
  if (c.image.includes('node-exporter')) return ['node_exporter', '/bin/node_exporter'];
  if (c.image.includes('recommendation') || c.image.includes('email-worker')) return ['python3.12', '/usr/local/bin/python3.12'];
  if (c.image.includes('orders') || c.image.includes('storefront') || c.image.includes('auth') || c.image.includes('order-events')) return ['node', 'node'];
  if (c.image.includes('billing-legacy')) return ['java', '/usr/lib/jvm/java-8-openjdk/bin/java'];
  return [c.name, `/usr/local/bin/${c.name}`];
}
function mockProcesses(host: Host, containers: InfraContainer[]): HostProcess[] {
  const sys = (host.cluster ? SYSTEM_PROCS.node : SYSTEM_PROCS.vm).map(([exe, command, cpu, mem], i) => ({
    pid: 1 + i * 211 + (h(host.id + exe) % 97), exe, command, containerId: '', cpu, memoryBytes: mem * MiB, threads: 8 + (h(exe) % 40), fds: 30 + (h(host.id + exe) % 200),
  }));
  const app = containers.filter((c) => c.cpu > 0 || c.memoryBytes > 0).map((c) => {
    const [exe, command] = exeOf(c);
    return { pid: 2000 + (h(c.id) % 60000), exe, command, containerId: c.id, cpu: c.cpu * 0.97, memoryBytes: c.memoryBytes * 0.95, threads: 4 + (h(c.id + 't') % 60), fds: 20 + (h(c.id + 'f') % 900) };
  });
  return [...app, ...sys].sort((a, b) => b.cpu - a.cpu).slice(0, 20);
}

// ---------------------------------------------------------------- public api (sync, ADR-013)

/** Every host reporting through trooper-agent `host` mode (VMs and K8s nodes). Hydrated in place in api mode. */
export const hosts: Host[] = world.hosts;

/**
 * No agent data at all → screens show the install instructions. While the first hydrate runs in api mode
 * the lists are empty too, so "no data" is only claimed once nothing is in flight.
 */
export function noAgentData(): boolean {
  return hosts.length === 0 && k8sClusters.length === 0 && (!isApi || remoteStatus().pending === 0);
}

export function infraContainers(f: { host?: string; service?: string }): InfraContainer[] {
  if (isApi) return remote(`/infra/containers${qs({ host: f.host, service: f.service })}`, [] as InfraContainer[], (raw: InfraContainer[] | null) => (raw ?? []).map(mapInfraContainer));
  return world.containers.filter((c) => (!f.host || c.host === f.host) && (!f.service || c.service === f.service));
}

const mapHost = (x: Host): Host => ({
  ...x, cluster: x.cluster ?? '', node: x.node ?? '', status: asInfraStatus(x.status), spark: { cpu: x.spark?.cpu ?? [], memory: x.spark?.memory ?? [] },
  gpus: x.gpus ?? [],
});

/** "GPU 0 · NVIDIA A100" — the label of a GPU's series. */
export const gpuLabel = (g: HostGPU) => `GPU ${g.index}${g.model ? ` · ${g.model}` : ''}`;
const zeros = (range: TimeRange) => Array<number>(RANGE_POINTS[range].points).fill(0);

export function hostDetails(id: string, range: TimeRange): HostDetails | undefined {
  const base = hosts.find((x) => x.id === id);
  if (isApi) {
    type Raw = Host & {
      series?: { cpu?: InfraSeriesSet; memory?: InfraSeriesSet; disk?: InfraSeriesSet; network?: InfraSeriesSet; gpu?: InfraSeriesSet } | null;
      containerList?: InfraContainer[] | null; processes?: HostProcess[] | null; services?: string[] | null;
    };
    const fallback: HostDetails | undefined = base && {
      ...base, cpuSeries: zeros(range), memorySeries: zeros(range), diskSeries: zeros(range), netInSeries: zeros(range), netOutSeries: zeros(range),
      gpuSeries: base.gpus.map((g) => ({ id: `gpu${g.index}`, label: gpuLabel(g), data: zeros(range) })), containerList: [], processes: [], services: [],
    };
    return remote<HostDetails | undefined>(`/infra/hosts/${seg(id)}${qs({ range })}`, fallback, (r: Raw) => {
      const { series, containerList, processes, services, ...row } = r;
      const one = (s: InfraSeriesSet | undefined, id: string) => pickInfraSeries(s, s?.series?.find((x) => x.id === id) ? id : s?.series?.[0]?.id ?? id, range);
      const mapped = mapHost(row);
      return {
        ...mapped,
        gpuSeries: mapped.gpus.map((g) => {
          const id = `gpu${g.index}`;
          return { id, label: series?.gpu?.series?.find((x) => x.id === id)?.label ?? gpuLabel(g), data: pickInfraSeries(series?.gpu, id, range) };
        }),
        cpuSeries: one(series?.cpu, 'cpu'), memorySeries: one(series?.memory, 'memory'), diskSeries: one(series?.disk, 'disk'),
        netInSeries: pickInfraSeries(series?.network, 'in', range), netOutSeries: pickInfraSeries(series?.network, 'out', range),
        containerList: (containerList ?? []).map(mapInfraContainer), processes: processes ?? [], services: services ?? [],
      };
    });
  }
  if (!base) return undefined;
  const containerList = world.containers.filter((c) => c.host === id);
  const stale = base.status === 'stale';
  const cut = (a: number[]) => (stale ? a.map((v, i) => (i >= a.length - Math.ceil((NOW - base.lastSeen) / RANGE_POINTS[range].stepMs) ? 0 : v)) : a);
  return {
    ...base,
    cpuSeries: cut(series(`host-cpu-${id}`, range, { base: base.cpu, noise: 0.12, daily: 0.15 }).map((v) => Math.min(1, v))),
    memorySeries: cut(series(`host-mem-${id}`, range, { base: base.memory, noise: 0.02, daily: 0.03 }).map((v) => Math.min(1, v))),
    diskSeries: cut(series(`host-disk-${id}`, range, { base: base.disk, noise: 0.002, daily: 0 }).map((v) => Math.min(1, v))),
    netInSeries: cut(series(`host-in-${id}`, range, { base: base.netIn, noise: 0.2, daily: 0.3 })),
    netOutSeries: cut(series(`host-out-${id}`, range, { base: base.netOut, noise: 0.2, daily: 0.3 })),
    gpuSeries: base.gpus.map((g) => ({ id: `gpu${g.index}`, label: gpuLabel(g),
      data: cut(series(`host-gpu-${id}-${g.index}`, range, { base: g.utilization, noise: 0.15, daily: 0.1 }).map((v) => Math.min(1, Math.max(0, v)))) })),
    containerList, processes: mockProcesses(base, containerList),
    services: [...new Set(containerList.map((c) => c.service).filter(Boolean))].sort(),
  };
}

/** Pods, hosts, containers and serverless functions behind one service. */
export function serviceInfra(serviceId: string, range: TimeRange): ServiceInfra {
  if (isApi) {
    return remote(`/services/${seg(serviceId)}/infra${qs({ range })}`, { pods: [], hosts: [], containers: [], functions: [] } as ServiceInfra,
      (r: Partial<ServiceInfra> | null) => ({
        pods: (r?.pods ?? []).map(mapK8sPod), hosts: r?.hosts ?? [], containers: (r?.containers ?? []).map(mapInfraContainer), functions: r?.functions ?? [],
      }));
  }
  const pods = k8sPods({ service: serviceId }, range);
  const containers = world.containers.filter((c) => c.service === serviceId);
  const hostIds = [...new Set([...containers.map((c) => c.host), ...pods.map((p) => p.node)].filter(Boolean))];
  return {
    pods, containers,
    hosts: hostIds.map((id) => hosts.find((x) => x.id === id)).filter((x): x is Host => !!x).map((x) => ({ id: x.id, cpu: x.cpu, memory: x.memory })),
    functions: [],
  };
}

if (isApi) {
  registerHydrator(() => hydrateList(hosts, `/infra/hosts${qs({ range: '1h' })}`, (raw: Host[] | null) => (raw ?? []).map(mapHost)));
}
