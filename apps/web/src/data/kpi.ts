/**
 * KPIs: objectives on what users experience ("homepage loads fast", "checkout succeeds").
 * Alerts are defined on the KPI (error-budget burn), and when a KPI degrades the root cause
 * analysis walks its call tree down to the deepest node that explains the change.
 */
import { RANGE_POINTS, timestamps } from './apm';
import { endpointNode } from './graph';
import { hydrateList, mutate, qs, registerHydrator, remote, seg } from './remote';
import type { Kpi, KpiAlertRule, KpiSli, RcaNode, Severity, TimeRange } from './types';
import './api/ops'; // registers the monitors / incidents / load-test hydrators

/** KPIs of the project (`GET /kpis`, hydrated in place). */
export const kpis: Kpi[] = [];

// ---------------------------------------------------------------- call trees behind KPIs
/**
 * Call tree for a degraded KPI, or undefined when it's healthy. Endpoint KPIs walk the endpoint's live call graph;
 * page / journey / app-start KPIs get their root cause from the alert engine (incident.rootCause).
 */
export function kpiTree(kpi: Kpi): RcaNode | undefined {
  if (kpi.status === 'ok' || kpi.target.kind !== 'endpoint' || !kpi.target.endpointId) return undefined;
  return endpointNode(kpi.target.serviceId, kpi.target.endpointId, kpi.sli === 'latency' ? 'latency' : 'errors');
}

export const kpiUnit = (k: Kpi): 'ms' | 'ratio' => (k.metric?.unit ?? (k.sli === 'latency' ? 'ms' : 'ratio'));

// ---------------------------------------------------------------- api (ADR-013)
interface SeriesSet { timestamps: number[]; series: { id: string; data: number[] }[] }
type RawKpi = Omit<Kpi, 'threshold' | 'metric' | 'alerts'> & {
  threshold?: Kpi['threshold'] | null; metric?: Kpi['metric'] | null; noData?: boolean;
  alerts?: (Omit<KpiAlertRule, 'kind' | 'state'> & { kind: string; state: string })[] | null;
};
const mapKpi = (k: RawKpi): Kpi => ({
  ...k, threshold: k.threshold ?? undefined, metric: k.metric ?? undefined, degradedSince: k.degradedSince || undefined,
  window: k.window === '7d' ? '7d' : '28d', source: k.source === 'code' || k.source === 'template' ? k.source : 'manual',
  alerts: (k.alerts ?? []).map((a) => ({
    ...a, kind: a.kind === 'burn_rate' ? 'burn_rate' : 'sli_below', channels: a.channels ?? [],
    state: a.state === 'firing' || a.state === 'pending' ? a.state : 'ok',
  })),
});
registerHydrator(() => hydrateList(kpis, '/kpis', (raw: RawKpi[]) => raw.map(mapKpi)));

const kpiSet = (k: Kpi, range: TimeRange) => remote<SeriesSet | null>(`/kpis/${seg(k.id)}/series${qs({ range })}`, null);
function kpiData(k: Kpi, range: TimeRange, id: 'good' | 'total' | 'burnRate', empty: number): number[] {
  const n = timestamps(range).length;
  const d = kpiSet(k, range)?.series.find((x) => x.id === id)?.data ?? [];
  return d.length >= n ? d.slice(d.length - n) : [...Array(n - d.length).fill(empty), ...d];
}

export interface KpiDraft {
  name: string; description: string; owner: string;
  target: Kpi['target']; sli: KpiSli;
  good: { metric?: string; thresholdMs?: number; noError?: boolean; description?: string };
  objective: number; window: '7d' | '28d';
  alerts: { kind: 'burn_rate' | 'sli_below'; longWindow: string; shortWindow?: string; threshold: number; severity: Severity; channels: string[] }[];
}
/** Create a KPI (`POST /kpis`). Resolves to its id. */
export async function createKpi(d: KpiDraft): Promise<string> {
  const k = await mutate<{ id: string }>('POST', '/kpis', { ...d, source: 'manual' }, ['/kpis']);
  return k.id;
}

/** Share of good events over time (`GET /kpis/{id}/series`; 1 while loading). */
/** Share of good events over time (`GET /kpis/{id}/series`; 1 while loading). */
export function kpiSeries(k: Kpi, range: TimeRange): number[] {
  return kpiData(k, range, 'good', 1);
}

/** The KPI's headline metric (e.g. LCP p75) over time. The api serves no headline-metric series yet → empty. */
export function kpiMetricSeries(_k: Kpi, _range: TimeRange): number[] {
  return [];
}

/** Error budget left over the range, walked back from the current budget with the good / total series. */
export function kpiBudgetSeries(k: Kpi, range: TimeRange): number[] {
  // walk back from the current budget: each bucket's bad events use (bad / allowed) of the window's events
  const good = kpiData(k, range, 'good', 1), total = kpiData(k, range, 'total', 0);
  const bucketsPerWindow = ((k.window === '7d' ? 7 : 28) * 86400_000) / RANGE_POINTS[range].stepMs;
  const windowEvents = (total.reduce((a, b) => a + b, 0) / (total.length || 1)) * bucketsPerWindow;
  const allowed = 1 - k.objective;
  const out = Array<number>(good.length);
  let acc = k.budgetRemaining;
  for (let i = good.length - 1; i >= 0; i--) {
    out[i] = Math.min(1, acc);
    if (windowEvents > 0 && allowed > 0) acc += ((1 - good[i]) * total[i]) / (allowed * windowEvents);
  }
  return out;
}

/** KPIs that target the service (call trees are not walked here: that would fetch every KPI's call graph). */
export function kpisTouching(serviceId: string): Kpi[] {
  return kpis.filter((k) => k.target.serviceId === serviceId);
}
