/**
 * APM data (services, endpoints, RED series, dependencies, callers, deploys, traces) read from the api
 * (ADR-013, ADR-028). Screens read only through `src/data/index.ts`. Fallbacks are empty lists / zero series.
 */
import type {
  Caller, CallerTraffic, Dependency, Endpoint, EndpointCall, Incident, InventoryItem, LoadTestRun, LoadTestScenario, LogLine, Monitor,
  RumPage, Repo, Service, Session, Suggestion, TimeBreakdown, TimeCategory, TimeRange,
} from './types';
import { hydrateList, registerHydrator, remote, seg } from './remote';
import * as A from './api/apm';

export const RANGE_POINTS: Record<TimeRange, { points: number; stepMs: number }> = {
  '15m': { points: 15, stepMs: 60_000 },
  '30m': { points: 30, stepMs: 60_000 },
  '1h': { points: 60, stepMs: 60_000 },
  '6h': { points: 72, stepMs: 5 * 60_000 },
  '24h': { points: 96, stepMs: 15 * 60_000 },
  '7d': { points: 84, stepMs: 2 * 3600_000 },
};

/** Bucket timestamps ending now — the chart axis until the api has answered for this range. */
function axis(range: TimeRange): number[] {
  const { points, stepMs } = RANGE_POINTS[range];
  const end = Math.floor(Date.now() / stepMs) * stepMs;
  return Array.from({ length: points }, (_, i) => end - (points - 1 - i) * stepMs);
}

// ---------- registries (hydrated in place from the api; empty until then) ----------
export const services: Service[] = [];
export const rumPages: RumPage[] = [];
export const sessions: Session[] = [];
export const logs: LogLine[] = [];
export const monitors: Monitor[] = [];
export const incidents: Incident[] = [];
export const suggestions: Suggestion[] = [];
export const scenarios: LoadTestScenario[] = [];
export const runs: LoadTestRun[] = [];
export const repos: Repo[] = [];
export const inventory: InventoryItem[] = [];

export const TIME_CATEGORIES: { id: TimeCategory; label: string; slot: number }[] = [
  { id: 'app', label: 'App code', slot: 1 },
  { id: 'db', label: 'Database', slot: 2 },
  { id: 'cache', label: 'Cache', slot: 3 },
  { id: 'http', label: 'External HTTP', slot: 7 },
  { id: 'queue', label: 'Queue publish', slot: 5 },
  // LLM calls (dep_kind `ai`, ADR-023) — next fixed slot in LINE_SLOTS order
  { id: 'ai', label: 'AI / LLM', slot: 6 },
];

/** Callers seen in api responses, in first-seen order (stable color order inside a page). */
export const CALLERS: Caller[] = [];

const aggregateCallers = (eps: Endpoint[], callersOf: (e: Endpoint) => CallerTraffic[]): (CallerTraffic & { topEndpoint: Endpoint })[] => {
  const total = eps.reduce((a, e) => a + e.rpm, 0) || 1;
  const acc = new Map<string, { caller: Caller; rpm: number; errW: number; p95W: number; top: { ep: Endpoint; rpm: number } }>();
  for (const e of eps) {
    for (const c of callersOf(e)) {
      const cur = acc.get(c.caller.id) ?? { caller: c.caller, rpm: 0, errW: 0, p95W: 0, top: { ep: e, rpm: 0 } };
      cur.rpm += c.rpm;
      cur.errW += c.errorRate * c.rpm;
      cur.p95W += c.p95 * c.rpm;
      if (c.rpm > cur.top.rpm) cur.top = { ep: e, rpm: c.rpm };
      acc.set(c.caller.id, cur);
    }
  }
  return [...acc.values()]
    .map((v) => ({ caller: v.caller, rpm: v.rpm, share: v.rpm / total, errorRate: v.errW / (v.rpm || 1), p95: v.p95W / (v.rpm || 1), topEndpoint: v.top.ep }))
    .sort((a, b) => b.rpm - a.rpm);
};

/** Chart buckets for a range: the bucket timestamps of the last api series for that range, else an axis ending now. */
export function timestamps(range: TimeRange): number[] {
  const t = A.apiTimestamps(range);
  if (t && t.length === RANGE_POINTS[range].points) return t;
  return axis(range);
}
const zeros = (range: TimeRange) => new Array<number>(RANGE_POINTS[range].points).fill(0);
const H1 = A.rangeQs('1h');

/** "svc" or "svc-endpointId" (the key screens use for series) → ids. Service ids may contain dashes. */
function parseKey(key: string): { serviceId: string; endpointId?: string } {
  if (services.some((s) => s.id === key)) return { serviceId: key };
  const s = services.filter((x) => key.startsWith(`${x.id}-`)).sort((a, b) => b.id.length - a.id.length)[0];
  return s ? { serviceId: s.id, endpointId: key.slice(s.id.length + 1) } : { serviceId: key };
}
const scopePath = (k: { serviceId: string; endpointId?: string }) => (k.endpointId ? A.epPath(k.serviceId, k.endpointId) : A.svcPath(k.serviceId));

// ---------- endpoints ----------
export function endpointsFor(serviceId: string): Endpoint[] {
  const svc = services.find((s) => s.id === serviceId);
  if (svc && (svc.type === 'web' || svc.type === 'mobile')) return [];
  return A.list(`${A.svcPath(serviceId)}/endpoints${H1}`, A.mapEndpoints);
}

// ---------- RED series ----------
const isClient = (id: string) => { const t = services.find((s) => s.id === id)?.type; return t === 'web' || t === 'mobile'; };
export function serviceTrafficSeries(serviceId: string, range: TimeRange): number[] {
  // web / mobile apps: /services/{app}/series only covers spans (zeros) → RUM views per minute
  if (isClient(serviceId)) return A.pick(A.seriesSet(`/apps/${seg(serviceId)}/series${A.rangeQs(range)}`, range), 'views', zeros(range).length);
  return A.pick(A.seriesSet(`${A.svcPath(serviceId)}/series${A.rangeQs(range)}`, range), 'rpm', zeros(range).length);
}
export function serviceErrorSeries(serviceId: string, range: TimeRange): number[] {
  if (isClient(serviceId)) return A.pick(A.seriesSet(`/apps/${seg(serviceId)}/series${A.rangeQs(range)}`, range), 'errorRate', zeros(range).length);
  return A.pick(A.seriesSet(`${A.svcPath(serviceId)}/series${A.rangeQs(range)}`, range), 'errorRate', zeros(range).length);
}
export function endpointTrafficSeries(ep: Endpoint, range: TimeRange): number[] {
  return A.pick(A.seriesSet(`${A.epPath(ep.serviceId, ep.id)}/series${A.rangeQs(range)}`, range), 'rpm', zeros(range).length);
}
export function endpointErrorSeries(ep: Endpoint, range: TimeRange): number[] {
  return A.pick(A.seriesSet(`${A.epPath(ep.serviceId, ep.id)}/series${A.rangeQs(range)}`, range), 'errorRate', zeros(range).length);
}

export type RedSeriesId = 'rpm' | 'errorRate' | 'p50' | 'p95' | 'p99' | 's2xx' | 's3xx' | 's4xx' | 's5xx';
/**
 * Full RED series (traffic, error rate, latency percentiles, status classes) of the project (no args), a service
 * or one endpoint. Consumers' endpoint series also carry `retried` / `dlq` (returned under s4xx / s5xx = 0).
 */
export function redSeries(range: TimeRange, serviceId?: string, endpointId?: string): Record<RedSeriesId, number[]> {
  const ids: RedSeriesId[] = ['rpm', 'errorRate', 'p50', 'p95', 'p99', 's2xx', 's3xx', 's4xx', 's5xx'];
  const path = serviceId ? `${scopePath({ serviceId, endpointId })}/series` : '/overview/series';
  const set = A.seriesSet(`${path}${A.rangeQs(range)}`, range);
  return Object.fromEntries(ids.map((id) => [id, A.pick(set, id, zeros(range).length)])) as Record<RedSeriesId, number[]>;
}

// ---------- dependencies, calls, time breakdown ----------
export function dependenciesFor(serviceId: string): Dependency[] {
  return A.list(`${A.svcPath(serviceId)}/dependencies${H1}`, A.mapDependencies);
}
export function endpointCalls(ep: Endpoint): EndpointCall[] {
  return A.list(`${A.epPath(ep.serviceId, ep.id)}/calls${H1}`, A.mapCalls);
}
/** While loading / without spans: the known average, nothing attributed to a category yet. */
const loadingBreakdown = (avg: number): TimeBreakdown => ({ avgMs: avg, parts: { app: 0, db: 0, cache: 0, http: 0, queue: 0, ai: 0 } });
export function endpointBreakdown(ep: Endpoint): TimeBreakdown {
  return remote(`${A.epPath(ep.serviceId, ep.id)}/breakdown${H1}`, loadingBreakdown(ep.p50), A.mapBreakdown);
}
export function serviceBreakdown(serviceId: string): TimeBreakdown {
  const s = services.find((x) => x.id === serviceId);
  return remote(`${A.svcPath(serviceId)}/breakdown${H1}`, loadingBreakdown((s?.p95 ?? 0) * 0.5), A.mapBreakdown);
}
/** `key` = service id or `${serviceId}-${endpointId}`. `b` / `critical` are kept for call-site compatibility. */
export function breakdownSeries(key: string, _b: TimeBreakdown, range: TimeRange, _critical: boolean): Record<TimeCategory, number[]> {
  const set = A.seriesSet(`${scopePath(parseKey(key))}/breakdown/series${A.rangeQs(range)}`, range);
  return Object.fromEntries(A.TIME_PARTS.map((k) => [k, A.pick(set, k, zeros(range).length)])) as Record<TimeCategory, number[]>;
}

const depSeries = (d: Dependency, range: TimeRange) =>
  A.seriesSet(`${A.svcPath(d.serviceId)}/dependencies/${seg(d.id)}/series${A.rangeQs(range)}`, range);
export function outgoingSeries(dep: Dependency, range: TimeRange): number[] {
  return A.pick(depSeries(dep, range), 'calls', zeros(range).length);
}
export function depErrorSeries(d: Dependency, range: TimeRange): number[] {
  return A.pick(depSeries(d, range), 'errorRate', zeros(range).length);
}
export function depLatencySeries(d: Dependency, range: TimeRange, stat: 'avg' | 'p95'): number[] {
  return A.pick(depSeries(d, range), stat, zeros(range).length);
}

// ---------- callers ----------
const strip = (r: A.CallerRow): CallerTraffic => ({ caller: r.caller, rpm: r.rpm, share: r.share, errorRate: r.errorRate, p95: r.p95 });

export function endpointCallers(ep: Endpoint): CallerTraffic[] {
  return A.list(`${A.epPath(ep.serviceId, ep.id)}/callers${H1}`, A.mapCallers(CALLERS)).map(strip);
}

export function serviceCallers(serviceId: string): (CallerTraffic & { topEndpoint: Endpoint })[] {
  const eps = endpointsFor(serviceId);
  const rows = A.list(`${A.svcPath(serviceId)}/callers${H1}`, A.mapCallers(CALLERS));
  // the api has no service-level sources for consumers (only per process) → sum the processes' sources
  if (rows.length === 0 && eps.some((e) => e.sub)) return aggregateCallers(eps, endpointCallers);
  const placeholder: Endpoint = { id: '', serviceId, method: '', route: '', rpm: 0, errorRate: 0, p50: 0, p95: 0, p99: 0, monitored: false, criticality: 'low' };
  return rows.map((r) => ({ ...strip(r), topEndpoint: eps.find((e) => e.id === r.topEndpointId) ?? r.topEndpointRaw ?? placeholder }));
}

/** `targetKey` = service id or `${serviceId}-${endpointId}`. */
export function callerSeries(targetKey: string, c: CallerTraffic, range: TimeRange): number[] {
  const k = parseKey(targetKey);
  if (c.caller.kind === 'topic' || c.caller.kind === 'queue' || c.caller.kind === 'schedule') {
    // a consumer's "caller" is its topic: the api has no caller series for it → messages of the processes reading it
    const eps = endpointsFor(k.serviceId).filter((e) => (!k.endpointId || e.id === k.endpointId) && e.sub?.topic === c.caller.name);
    return eps.map((e) => endpointTrafficSeries(e, range)).reduce((a, v) => a.map((x, i) => x + (v[i] ?? 0)), zeros(range));
  }
  const set = A.seriesSet(`${scopePath(k)}/callers/series${A.rangeQs(range)}`, range);
  return A.pick(set, c.caller.id, zeros(range).length);
}

// ---------- deploys & traces ----------
export interface ServiceDeploy { service: string; env: string; version: string; ts: number }
export function deploysFor(serviceId: string, range: TimeRange): ServiceDeploy[] {
  return A.list(`${A.svcPath(serviceId)}/deploys${A.rangeQs(range)}`, (raw: A.RawDeploy[] | null) => (raw ?? []).map((d) => ({ ...d })));
}
export type TraceSummary = A.RawTraceSummary;
export function tracesFor(serviceId: string, ep: Endpoint | undefined, range: TimeRange, opts: { errors?: boolean; sort?: 'recent' | 'slowest'; limit?: number } = {}): TraceSummary[] {
  const s = services.find((x) => x.id === serviceId);
  return A.list(`/traces${A.rangeQs(range, {
    service: s?.name ?? serviceId, endpoint: ep ? (ep.sub ? ep.sub.process : `${ep.method} ${ep.route}`) : undefined,
    errors: opts.errors ? 'true' : undefined, sort: opts.sort, limit: opts.limit ?? 20,
  })}`, (raw: TraceSummary[] | null) => raw ?? []);
}

/**
 * Recent traces served by one Kubernetes pod (contracts §22: GET /traces?service=&pod=&namespace=, span resource
 * attributes k8s.pod.name / k8s.namespace.name).
 */
export function podTraces(pod: { name: string; namespace: string; service: string }, range: TimeRange, opts: { errors?: boolean; limit?: number } = {}): TraceSummary[] {
  const limit = opts.limit ?? 20;
  return A.list(`/traces${A.rangeQs(range, {
    service: pod.service || undefined, pod: pod.name, namespace: pod.namespace || undefined, errors: opts.errors ? 'true' : undefined, limit,
  })}`, (raw: TraceSummary[] | null) => raw ?? []);
}

// ---------- list hydration ----------
registerHydrator(() => hydrateList(services, `/services${H1}`, A.mapServices));
