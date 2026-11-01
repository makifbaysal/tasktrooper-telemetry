/**
 * Hosts, containers and processes (ADR-019, .ai/contracts.md §20) — trooper-agent `host` mode.
 * A host is a VM or a Kubernetes node; containers come from kubelet (K8s) or the Docker socket (VMs).
 * Data: /infra/hosts, /infra/containers.
 */
import { RANGE_POINTS } from './apm';
import { hydrateList, qs, registerHydrator, remote, remoteStatus, seg } from './remote';
import { asInfraStatus, k8sClusters, mapInfraContainer, mapK8sPod, pickInfraSeries } from './k8s';
import type { Host, HostDetails, HostGPU, HostProcess, InfraContainer, InfraSeriesSet, ServiceInfra, TimeRange } from './types';

// ---------------------------------------------------------------- public api (sync, ADR-013)

/** Every host reporting through trooper-agent `host` mode (VMs and K8s nodes). Hydrated in place. */
export const hosts: Host[] = [];

/**
 * No agent data at all → screens show the install instructions. While the first hydrate runs
 * the lists are empty too, so "no data" is only claimed once nothing is in flight.
 */
export function noAgentData(): boolean {
  return hosts.length === 0 && k8sClusters.length === 0 && remoteStatus().pending === 0;
}

export function infraContainers(f: { host?: string; service?: string }): InfraContainer[] {
  return remote(`/infra/containers${qs({ host: f.host, service: f.service })}`, [] as InfraContainer[], (raw: InfraContainer[] | null) => (raw ?? []).map(mapInfraContainer));
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

/** Pods, hosts, containers and serverless functions behind one service. */
export function serviceInfra(serviceId: string, range: TimeRange): ServiceInfra {
  return remote(`/services/${seg(serviceId)}/infra${qs({ range })}`, { pods: [], hosts: [], containers: [], functions: [] } as ServiceInfra,
    (r: Partial<ServiceInfra> | null) => ({
      pods: (r?.pods ?? []).map(mapK8sPod), hosts: r?.hosts ?? [], containers: (r?.containers ?? []).map(mapInfraContainer), functions: r?.functions ?? [],
    }));
}

registerHydrator(() => hydrateList(hosts, `/infra/hosts${qs({ range: '1h' })}`, (raw: Host[] | null) => (raw ?? []).map(mapHost)));
