/**
 * Serverless functions (ADR-020, .ai/contracts.md §21): AWS Lambda functions and Azure Function Apps discovered by the
 * cloud integrations, with CloudWatch / Azure Monitor metrics, cold starts from spans and an estimated cost.
 *
 * Data: `GET /serverless/summary`, `GET /serverless/functions`, `GET /serverless/functions/{id}` (all `?range=`).
 */
import { qs, remote, seg } from './remote';
import type { SeriesSet } from './api/apm';
import type { TimeRange } from './types';

// ---------------------------------------------------------------- types

export type FaasProvider = 'aws' | 'azure';
export type FaasStatus = 'ok' | 'warning' | 'critical' | 'idle' | 'missing';
export const FAAS_PROVIDER_LABEL: Record<FaasProvider, string> = { aws: 'AWS Lambda', azure: 'Azure Functions' };

export interface FaasFunction {
  id: string;
  /** aws-<region>-<name> | azure-<app> */
  slug: string;
  name: string;
  provider: FaasProvider;
  /** Azure: function app name (= name) */
  app?: string;
  region: string;
  runtime: string;
  memoryMb?: number;
  timeoutS?: number;
  arch: string;
  /** 'api', 'sqs:orders', 'schedule', 'http', 'kinesis:…' */
  triggers: string[];
  /** span service.name the function reports as */
  service: string;
  invocations: number;
  errors: number;
  errorRate: number;
  throttles: number;
  p95Ms: number;
  p99Ms: number;
  coldStarts: number;
  coldStartRate: number;
  concurrencyMax: number;
  /** estimate, USD, for the selected range */
  costUsd: number;
  status: FaasStatus;
  /** invocations per range bucket */
  spark: number[];
  /** Azure per-function row (App Insights, contracts §22): id of its Function App row */
  parentId?: string;
  /** Azure Function App row: whether per-function metrics come from Application Insights */
  appInsights?: 'connected' | 'unavailable';
}

export interface FaasSummary {
  functions: number; invocations: number; errors: number; errorRate: number; throttles: number; coldStarts: number; costUsd: number;
  byProvider: { provider: FaasProvider; functions: number; invocations: number; costUsd: number }[];
}

export interface FaasAppFunction { name: string; trigger?: string; disabled?: boolean }
export interface FaasRecentError { ts: number; traceId: string; message: string }
export interface FaasDetail {
  fn: FaasFunction;
  /** ids: invocations, errors, throttles */
  invocations: SeriesSet;
  /** ms; ids: avg, p95, p99 */
  duration: SeriesSet;
  concurrency: SeriesSet;
  coldStarts: SeriesSet;
  recentErrors: FaasRecentError[];
  /** Azure: functions inside the app (`meta.functions`) */
  functions?: FaasAppFunction[];
  /** Azure Function App: its per-function rows (`children`) */
  children?: FaasFunction[];
  integration?: { id: string; name: string };
}

// ---------------------------------------------------------------- helpers

const num = (v: unknown, d = 0): number => (typeof v === 'number' && Number.isFinite(v) ? v : d);
const arr = <T>(v: T[] | null | undefined): T[] => (Array.isArray(v) ? v : []);
const STATUSES: FaasStatus[] = ['ok', 'warning', 'critical', 'idle', 'missing'];
const toMs = (v: unknown): number => {
  if (typeof v === 'number') return v < 1e12 ? v * 1000 : v;
  if (typeof v === 'string') { const t = Date.parse(v); return Number.isFinite(t) ? t : 0; }
  return 0;
};

/** Contract status rule (§21); the api computes its own, this one serves client-side checks. */
export function faasStatus(f: Pick<FaasFunction, 'invocations' | 'errorRate' | 'throttles' | 'p95Ms' | 'timeoutS'>): FaasStatus {
  if (f.invocations === 0) return 'idle';
  if (f.errorRate > 0.05 || f.throttles > 0) return 'critical';
  if (f.errorRate > 0.01 || (f.timeoutS && f.p95Ms > f.timeoutS * 1000 * 0.8)) return 'warning';
  return 'ok';
}

/** Rows that need a look first: critical, then warning. */
export const faasRank = (s: FaasStatus) => ({ critical: 4, warning: 3, missing: 2, ok: 1, idle: 0 })[s];

/**
 * Summary rule (contracts §22, api README): Azure per-function rows are left out of the metric sums (the app row carries
 * the app-level totals) and an app with per-function rows counts as its functions, not as one more.
 */
export function summaryRows(rows: FaasFunction[]): { countFn: boolean[]; addMetrics: boolean[] } {
  const ids = new Set(rows.map((r) => r.id));
  const hasChild = new Set(rows.filter((r) => r.parentId && ids.has(r.parentId)).map((r) => r.parentId!));
  return { countFn: rows.map((r) => !hasChild.has(r.id)), addMetrics: rows.map((r) => !r.parentId) };
}

// ---------------------------------------------------------------- api mapping

type RawFn = Partial<Omit<FaasFunction, 'id' | 'triggers' | 'spark'>> & {
  id: string | number; triggers?: string[] | null; spark?: number[] | null; memoryMb?: number | null; timeoutS?: number | null; app?: string | null;
  parentId?: string | number | null; appInsights?: string | null;
};
function mapFn(r: RawFn): FaasFunction {
  const provider: FaasProvider = r.provider === 'azure' ? 'azure' : 'aws';
  return {
    id: String(r.id), slug: r.slug ?? String(r.id), name: r.name ?? r.slug ?? String(r.id), provider, app: r.app ?? undefined,
    region: r.region ?? '', runtime: r.runtime ?? '', memoryMb: r.memoryMb ?? undefined, timeoutS: r.timeoutS ?? undefined, arch: r.arch ?? '',
    triggers: arr(r.triggers), service: r.service ?? r.name ?? '',
    invocations: num(r.invocations), errors: num(r.errors), errorRate: num(r.errorRate), throttles: num(r.throttles),
    p95Ms: num(r.p95Ms), p99Ms: num(r.p99Ms), coldStarts: num(r.coldStarts), coldStartRate: num(r.coldStartRate),
    concurrencyMax: num(r.concurrencyMax), costUsd: num(r.costUsd),
    status: STATUSES.includes(r.status as FaasStatus) ? (r.status as FaasStatus) : 'ok',
    spark: arr(r.spark).map((v) => num(v)),
    parentId: r.parentId != null && r.parentId !== '' ? String(r.parentId) : undefined,
    appInsights: r.appInsights === 'connected' || r.appInsights === 'unavailable' ? r.appInsights : undefined,
  };
}
interface RawSet { timestamps?: number[] | null; series?: { id: string; label: string; data: number[] | null }[] | null }
const mapSet = (s: RawSet | null | undefined): SeriesSet => ({
  timestamps: arr(s?.timestamps).map(toMs),
  series: arr(s?.series).map((x) => ({ id: x.id, label: x.label, data: arr(x.data).map((v) => num(v)) })),
});
interface RawDetailSeries { invocations?: RawSet | null; duration?: RawSet | null; concurrency?: RawSet | null; coldStarts?: RawSet | null }
/** contracts §21: list-row fields unchanged + `series` + `recentErrors`, `functions?`, `integration`. */
type RawDetail = RawFn & {
  series?: RawDetailSeries | null;
  recentErrors?: { ts: number | string; traceId: string; message: string }[] | null;
  functions?: (string | { name: string; trigger?: string | null; disabled?: boolean | null })[] | null;
  meta?: { functions?: (string | { name: string; trigger?: string | null; disabled?: boolean | null })[] | null } | null;
  integration?: { id: string | number; name: string } | null;
  children?: RawFn[] | null;
};
function mapDetail(r: RawDetail): FaasDetail {
  const s = r.series ?? {};
  const inv = mapSet(s.invocations);
  const fn = mapFn(r);
  if (!fn.spark.length) fn.spark = inv.series.find((x) => x.id === 'invocations')?.data ?? [];
  return {
    fn, invocations: inv, duration: mapSet(s.duration), concurrency: mapSet(s.concurrency), coldStarts: mapSet(s.coldStarts),
    recentErrors: arr(r.recentErrors).map((e) => ({ ts: toMs(e.ts), traceId: e.traceId, message: e.message })),
    functions: (r.functions ?? r.meta?.functions ?? undefined)?.map((x) => (typeof x === 'string' ? { name: x } : { name: x.name, trigger: x.trigger ?? undefined, disabled: x.disabled ?? undefined })),
    children: r.children ? arr(r.children).map(mapFn) : undefined,
    integration: r.integration ? { id: String(r.integration.id), name: r.integration.name } : undefined,
  };
}
type RawSummary = Partial<Omit<FaasSummary, 'byProvider'>> & { byProvider?: { provider: string; functions?: number; invocations?: number; costUsd?: number }[] | null };
const mapSummary = (r: RawSummary | null): FaasSummary => ({
  functions: num(r?.functions), invocations: num(r?.invocations), errors: num(r?.errors), errorRate: num(r?.errorRate),
  throttles: num(r?.throttles), coldStarts: num(r?.coldStarts), costUsd: num(r?.costUsd),
  byProvider: arr(r?.byProvider).map((p) => ({ provider: p.provider === 'azure' ? 'azure' : 'aws', functions: num(p.functions), invocations: num(p.invocations), costUsd: num(p.costUsd) })),
});
const EMPTY_SUMMARY: FaasSummary = { functions: 0, invocations: 0, errors: 0, errorRate: 0, throttles: 0, coldStarts: 0, costUsd: 0, byProvider: [] };

// ---------------------------------------------------------------- reads

/** `GET /serverless/summary?range=` */
export function serverlessSummary(range: TimeRange): FaasSummary {
  return remote(`/serverless/summary${qs({ range })}`, EMPTY_SUMMARY, (r: RawSummary | null) => mapSummary(r));
}

/** `GET /serverless/functions?range=` — provider / search filtering happens client-side (one cached list per range). */
export function serverlessFunctions(range: TimeRange): FaasFunction[] {
  return remote<FaasFunction[]>(`/serverless/functions${qs({ range })}`, [], (r: RawFn[] | null) => arr(r).map(mapFn));
}

/** `GET /serverless/functions/{id}?range=` — undefined while loading or when unknown. */
export function serverlessFunction(id: string, range: TimeRange): FaasDetail | undefined {
  return remote<FaasDetail | undefined>(`/serverless/functions/${seg(id)}${qs({ range })}`, undefined, (r: RawDetail | null) => (r ? mapDetail(r) : undefined));
}

/** Functions whose span `service` is this service (ServiceDetail link). */
export function functionsOfService(serviceId: string, range: TimeRange = '1h'): FaasFunction[] {
  return serverlessFunctions(range).filter((f) => f.service === serviceId);
}
