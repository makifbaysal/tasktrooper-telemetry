/**
 * Api mode (ADR-013) for APM, RUM and logs: raw api shapes (services/api/openapi.yaml) → UI types (data/types.ts).
 * The api follows types.ts closely; these mappers absorb the differences (nullable fields, extra fields,
 * open string enums, missing time-breakdown parts) so screens never see an unexpected value.
 */
import { qs, remote, seg } from '../remote';
import type {
  Caller, CallerKind, CallerTraffic, Criticality, Dependency, Endpoint, EndpointCall, Health, LogLine, RumPage, Service, ServiceType,
  Session, Subscription, TimeBreakdown, TimeCategory, TimeRange,
} from '../types';

// ---------------------------------------------------------------- raw shapes
export interface RawSeries { id: string; label: string; data: number[] | null }
export interface RawSeriesSet { timestamps: number[] | null; series: RawSeries[] | null }
export interface SeriesSet { timestamps: number[]; series: { id: string; label: string; data: number[] }[] }

type RawService = Omit<Service, 'lag' | 'type' | 'status'> & { type: string; status: string; lag?: number | null; spark?: number[] };
type RawEndpoint = Omit<Endpoint, 'sub' | 'criticality'> & { criticality: string; sub?: (Omit<Subscription, 'source'> & { source: string }) | null; status?: unknown; spark?: number[] };
type RawDependency = Omit<Dependency, 'kind'> & { kind: string };
type RawCaller = { caller: { id: string; name: string; kind: string; via: string }; rpm: number; share: number; errorRate: number; p95: number; topEndpoint?: RawEndpoint | null };
type RawCall = { dep: RawDependency; callsPerReq: number; msPerReq: number };
type RawLogLine = { id: string; ts: number; level: string; service: string; message: string; traceId?: string | null; route?: string | null; source?: string | null; attrs?: Record<string, unknown> | null };
export interface RawLogPage { lines: RawLogLine[] | null; nextBefore: number; levels: Record<string, number> | null; services: Record<string, number> | null }
export interface RawLogPattern { id: string; pattern: string; level: string; services: string[] | null; count: number; last: number }
export interface RawVital { id: string; label: string; value: number; good: number; poor: number; unit: string }
export interface RawAppSummary {
  activeUsers: number; prevActiveUsers: number; sessions: number; pageViewsPerMin: number; avgSessionSec: number;
  errorRate: number; prevErrorRate: number; vitals: RawVital[] | null;
}
export interface RawAppApiCall { endpoint: RawEndpoint; service: RawService; rpm: number; errorRate: number; p95: number }
export interface RawTraceSummary { id: string; ts: number; dur: number; status: number; service: string; method: string; route: string; isError: boolean }
export interface RawDeploy { service: string; env: string; version: string; ts: number }
export interface RawRumError {
  fingerprint: string; type: string; message: string; view: string; count: number; sessions: number; crash: boolean;
  firstSeen: number; lastSeen: number; appVersion: string;
}

// ---------------------------------------------------------------- helpers
const num = (v: unknown, d = 0): number => (typeof v === 'number' && Number.isFinite(v) ? v : d);
const arr = <T>(v: T[] | null | undefined): T[] => (Array.isArray(v) ? v : []);
const oneOf = <T extends string>(v: string, allowed: readonly T[], d: T): T => ((allowed as readonly string[]).includes(v) ? (v as T) : d);

export const rangeQs = (range: TimeRange, extra: Record<string, string | number | undefined> = {}) => qs({ range, ...extra });
export const svcPath = (serviceId: string) => `/services/${seg(serviceId)}`;
export const epPath = (serviceId: string, endpointId: string) => `${svcPath(serviceId)}/endpoints/${seg(endpointId)}`;

// ---------------------------------------------------------------- series
/** Last bucket timestamps the api returned per range — keeps `timestamps(range)` aligned with api series. */
const lastTs: Partial<Record<TimeRange, number[]>> = {};
export const apiTimestamps = (range: TimeRange): number[] | undefined => lastTs[range];

/** A SeriesSet for `path` (cached, see remote()). Undefined while loading or after an error. */
export function seriesSet(path: string, range: TimeRange): SeriesSet | undefined {
  return remote<SeriesSet | undefined>(path, undefined, (raw: RawSeriesSet) => {
    const ts = arr(raw?.timestamps);
    if (ts.length) lastTs[range] = ts;
    return { timestamps: ts, series: arr(raw?.series).map((s) => ({ id: s.id, label: s.label, data: arr(s.data).map((v) => num(v)) })) };
  });
}

/** One series of a set, or zeros of length `n` (the chart's bucket count) while it loads / when absent. */
export function pick(set: SeriesSet | undefined, id: string, n: number): number[] {
  const s = set?.series.find((x) => x.id === id);
  if (!s) return new Array<number>(n).fill(0);
  if (s.data.length === n) return s.data;
  // keep the chart's bucket count stable even if a set has a different length
  return s.data.length > n ? s.data.slice(-n) : [...new Array<number>(n - s.data.length).fill(0), ...s.data];
}

// ---------------------------------------------------------------- entities
const SERVICE_TYPES = ['http', 'worker', 'consumer', 'web', 'mobile'] as const;
const HEALTH = ['ok', 'warning', 'critical'] as const;
const CRIT = ['critical', 'important', 'low'] as const;
const SOURCES = ['kafka', 'rabbitmq', 'sqs', 'cron'] as const;
const DEP_KINDS = ['db', 'cache', 'http', 'queue', 'ai'] as const;
const CALLER_KINDS = ['service', 'web', 'mobile', 'partner', 'webhook', 'probe', 'unknown', 'topic', 'queue', 'schedule'] as const;
export const TIME_PARTS: TimeCategory[] = ['app', 'db', 'cache', 'http', 'queue', 'ai'];

export function mapService(r: RawService): Service {
  const s: Service = {
    id: r.id, name: r.name || r.id, type: oneOf<ServiceType>(r.type, SERVICE_TYPES, 'http'), language: r.language ?? '', version: r.version ?? '',
    rpm: num(r.rpm), errorRate: num(r.errorRate), p95: num(r.p95), prevRpm: num(r.prevRpm), prevErrorRate: num(r.prevErrorRate), prevP95: num(r.prevP95),
    status: oneOf<Health>(r.status, HEALTH, 'ok'), instances: num(r.instances),
  };
  if (typeof r.lag === 'number') s.lag = r.lag;
  if (r.source === 'ebpf') s.source = 'ebpf';
  return s;
}
export const mapServices = (raw: RawService[] | null) => arr(raw).map(mapService);

export function mapEndpoint(r: RawEndpoint): Endpoint {
  const e: Endpoint = {
    id: r.id, serviceId: r.serviceId, method: r.method ?? '', route: r.route ?? '',
    rpm: num(r.rpm), errorRate: num(r.errorRate), p50: num(r.p50), p95: num(r.p95), p99: num(r.p99),
    monitored: !!r.monitored, criticality: oneOf<Criticality>(r.criticality, CRIT, 'low'),
  };
  if (r.sub) {
    const s = r.sub;
    e.sub = {
      process: s.process, source: oneOf(s.source, SOURCES, 'kafka'), topic: s.topic ?? '',
      lag: num(s.lag), retriesPerMin: num(s.retriesPerMin), dlqPerMin: num(s.dlqPerMin),
      ...(s.group ? { group: s.group } : {}), ...(s.partitions ? { partitions: s.partitions } : {}), ...(s.schedule ? { schedule: s.schedule } : {}),
    };
  }
  return e;
}
export const mapEndpoints = (raw: RawEndpoint[] | null) => arr(raw).map(mapEndpoint);

export function mapDependency(r: RawDependency): Dependency {
  return {
    id: r.id, serviceId: r.serviceId, name: r.name, kind: oneOf(r.kind, DEP_KINDS, 'http'),
    avgMs: num(r.avgMs), p95: num(r.p95), errorRate: num(r.errorRate), callsPerMin: num(r.callsPerMin),
  };
}
export const mapDependencies = (raw: RawDependency[] | null) => arr(raw).map(mapDependency);

export const mapCalls = (raw: RawCall[] | null): EndpointCall[] =>
  arr(raw).map((c) => ({ dep: mapDependency(c.dep), callsPerReq: num(c.callsPerReq), msPerReq: num(c.msPerReq) }));

export function mapBreakdown(raw: { avgMs: number; parts: Record<string, number> | null } | null): TimeBreakdown {
  const parts = {} as Record<TimeCategory, number>;
  for (const k of TIME_PARTS) parts[k] = num(raw?.parts?.[k]);
  return { avgMs: num(raw?.avgMs), parts };
}

/** Callers seen in api data, appended to the exported registry so caller colors stay stable across a page. */
export function mapCaller(r: RawCaller['caller'], registry: Caller[]): Caller {
  const c: Caller = { id: r.id, name: r.name || r.id, kind: oneOf<CallerKind>(r.kind, CALLER_KINDS, 'unknown'), via: r.via ?? '' };
  if (!registry.some((x) => x.id === c.id)) registry.push(c);
  return c;
}
export type CallerRow = CallerTraffic & { topEndpointId?: string; topEndpointRaw?: Endpoint };
export const mapCallers = (registry: Caller[]) => (raw: RawCaller[] | null): CallerRow[] =>
  arr(raw).map((r) => ({
    caller: mapCaller(r.caller, registry), rpm: num(r.rpm), share: num(r.share), errorRate: num(r.errorRate), p95: num(r.p95),
    ...(r.topEndpoint ? { topEndpointId: r.topEndpoint.id, topEndpointRaw: mapEndpoint(r.topEndpoint) } : {}),
  }));

// ---------------------------------------------------------------- logs
const LEVELS = ['debug', 'info', 'warn', 'error'] as const;
export function level(l: string): LogLine['level'] {
  const v = (l ?? '').toLowerCase();
  if (v === 'fatal' || v === 'critical' || v === 'panic') return 'error';
  if (v === 'warning') return 'warn';
  if (v === 'trace') return 'debug';
  return oneOf(v, LEVELS, 'info');
}
export const mapLogLine = (r: RawLogLine): LogLine => ({
  id: r.id, ts: num(r.ts), level: level(r.level), service: r.service, message: r.message ?? '', ...(r.traceId ? { traceId: r.traceId } : {}),
  // Vercel log drains: attrs['vercel.source'] (contracts §16)
  ...((r.source || r.attrs?.['vercel.source']) ? { source: String(r.source || r.attrs?.['vercel.source']) } : {}),
});
export const mapLogPage = (raw: RawLogPage | null): LogLine[] => arr(raw?.lines).map(mapLogLine);

// ---------------------------------------------------------------- RUM
export function mapRumPage(r: RumPage): RumPage {
  return {
    id: r.id, path: r.path, views: num(r.views), users: num(r.users), avgTime: num(r.avgTime), lcp: num(r.lcp), inp: num(r.inp),
    cls: num(r.cls), errorRate: num(r.errorRate), bounce: num(r.bounce),
  };
}
export const emptyPage = (path = '/'): RumPage => ({ id: 'root', path, views: 0, users: 0, avgTime: 0, lcp: 0, inp: 0, cls: 0, errorRate: 0, bounce: 0 });
/** Never empty: the app screen indexes pages by modulo (errors, session timelines). */
export const mapRumPages = (raw: RumPage[] | null) => {
  const pages = arr(raw).map(mapRumPage);
  return pages.length ? pages : [emptyPage()];
};

export const mapSession = (r: Session): Session => ({
  id: r.id, user: r.user || 'anonymous', identified: !!r.identified, country: r.country || '—', device: r.device || '—',
  startedAt: num(r.startedAt), duration: Math.round(num(r.duration)), pages: num(r.pages), errors: num(r.errors), entry: r.entry ?? '', exit: r.exit ?? '', hasReplay: !!r.hasReplay,
});
export const mapSessions = (raw: Session[] | null) => arr(raw).map(mapSession);

export const mapVital = (v: RawVital) => ({
  id: v.id, label: v.label, value: num(v.value), good: num(v.good), poor: num(v.poor), unit: (v.unit === 'score' ? 'score' : 'ms') as 'ms' | 'score',
});

/** Load a list path through the cache (endpoint lists, callers …) with a fresh fallback array each call. */
export function list<T>(path: string, map: (raw: never) => T[]): T[] {
  return remote<T[]>(path, [], map);
}
