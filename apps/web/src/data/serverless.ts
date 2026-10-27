/**
 * Serverless functions (ADR-020, .ai/contracts.md §21): AWS Lambda functions and Azure Function Apps discovered by the
 * cloud integrations, with CloudWatch / Azure Monitor metrics, cold starts from spans and an estimated cost.
 *
 * Api mode: `GET /serverless/summary`, `GET /serverless/functions`, `GET /serverless/functions/{id}` (all `?range=`).
 * Mock mode: seven functions across both providers — one throttling, one close to its timeout, one idle — plus the three
 * per-function rows (App Insights, contracts §22) of the `shop-notifications` Azure app.
 */
import { NOW, RANGE_POINTS, series, timestamps } from './mock';
import { isApi, qs, remote, seg } from './remote';
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

/** Contract status rule (§21) — used by the mock; the api computes its own. */
export function faasStatus(f: Pick<FaasFunction, 'invocations' | 'errorRate' | 'throttles' | 'p95Ms' | 'timeoutS'>): FaasStatus {
  if (f.invocations === 0) return 'idle';
  if (f.errorRate > 0.05 || f.throttles > 0) return 'critical';
  if (f.errorRate > 0.01 || (f.timeoutS && f.p95Ms > f.timeoutS * 1000 * 0.8)) return 'warning';
  return 'ok';
}

/** Rows that need a look first: critical, then warning. */
export const faasRank = (s: FaasStatus) => ({ critical: 4, warning: 3, missing: 2, ok: 1, idle: 0 })[s];

// ---------------------------------------------------------------- mock

interface MockFn {
  slug: string; name: string; provider: FaasProvider; app?: string; region: string; runtime: string; memoryMb: number; timeoutS?: number;
  arch: string; triggers: string[]; service: string; integration: { id: string; name: string }; functions?: FaasAppFunction[];
  /** per-function row: the app's slug (meta.parent); no execution units → no cost */
  parent?: string; appInsights?: 'connected' | 'unavailable';
  /** invocations per minute */
  rpm: number; errorRate: number; avgMs: number; p95Ms: number; coldRate: number; throttlePerMin?: number; spikeAt?: number; idle?: boolean;
}
const AWS_INT = { id: 'int-aws', name: 'aws-prod' };
const AZ_INT = { id: 'int-azure', name: 'azure-shop' };
const MOCK_FNS: MockFn[] = [
  { slug: 'aws-eu-central-1-checkout-webhook', name: 'checkout-webhook', provider: 'aws', region: 'eu-central-1', runtime: 'nodejs22.x', memoryMb: 512, timeoutS: 30,
    arch: 'arm64', triggers: ['api'], service: 'checkout-webhook', integration: AWS_INT, rpm: 940, errorRate: 0.0018, avgMs: 84, p95Ms: 210, coldRate: 0.012 },
  { slug: 'aws-eu-central-1-order-processor', name: 'order-processor', provider: 'aws', region: 'eu-central-1', runtime: 'python3.12', memoryMb: 1024, timeoutS: 60,
    arch: 'x86_64', triggers: ['sqs:orders'], service: 'order-processor', integration: AWS_INT, rpm: 2600, errorRate: 0.0074, avgMs: 320, p95Ms: 940, coldRate: 0.021,
    throttlePerMin: 38, spikeAt: 0.82 },
  { slug: 'aws-eu-central-1-image-resize', name: 'image-resize', provider: 'aws', region: 'eu-central-1', runtime: 'nodejs22.x', memoryMb: 1536, timeoutS: 10,
    arch: 'arm64', triggers: ['s3:product-images'], service: 'image-resize', integration: AWS_INT, rpm: 120, errorRate: 0.0042, avgMs: 3900, p95Ms: 8300, coldRate: 0.06 },
  { slug: 'aws-eu-central-1-clickstream-enricher', name: 'clickstream-enricher', provider: 'aws', region: 'eu-central-1', runtime: 'java21', memoryMb: 2048, timeoutS: 120,
    arch: 'x86_64', triggers: ['kinesis:clickstream'], service: 'clickstream-enricher', integration: AWS_INT, rpm: 410, errorRate: 0.0006, avgMs: 640, p95Ms: 1300, coldRate: 0.004 },
  { slug: 'aws-us-east-1-nightly-report', name: 'nightly-report', provider: 'aws', region: 'us-east-1', runtime: 'python3.12', memoryMb: 3008, timeoutS: 900,
    arch: 'arm64', triggers: ['schedule'], service: 'nightly-report', integration: AWS_INT, rpm: 0, errorRate: 0, avgMs: 0, p95Ms: 0, coldRate: 0, idle: true },
  { slug: 'azure-shop-notifications', name: 'shop-notifications', app: 'shop-notifications', provider: 'azure', region: 'westeurope', runtime: 'node 20', memoryMb: 1536,
    arch: '', triggers: ['http', 'queue:notifications', 'timer'], service: 'shop-notifications', integration: AZ_INT,
    functions: [{ name: 'SendEmail', trigger: 'queue:notifications' }, { name: 'SendPush', trigger: 'http' }, { name: 'DailyDigest', trigger: 'timer', disabled: true }], rpm: 760, errorRate: 0.0031, avgMs: 140, p95Ms: 420, coldRate: 0.018,
    appInsights: 'connected' },
  // per-function rows of shop-notifications (Application Insights requests)
  { slug: 'azure-shop-notifications-SendEmail', name: 'SendEmail', app: 'shop-notifications', parent: 'azure-shop-notifications', provider: 'azure', region: 'westeurope',
    runtime: 'node 20', memoryMb: 1536, arch: '', triggers: ['queue:notifications'], service: 'shop-notifications', integration: AZ_INT,
    rpm: 470, errorRate: 0.0046, avgMs: 180, p95Ms: 510, coldRate: 0.02 },
  { slug: 'azure-shop-notifications-SendPush', name: 'SendPush', app: 'shop-notifications', parent: 'azure-shop-notifications', provider: 'azure', region: 'westeurope',
    runtime: 'node 20', memoryMb: 1536, arch: '', triggers: ['http'], service: 'shop-notifications', integration: AZ_INT,
    rpm: 290, errorRate: 0.0008, avgMs: 75, p95Ms: 190, coldRate: 0.015 },
  { slug: 'azure-shop-notifications-DailyDigest', name: 'DailyDigest', app: 'shop-notifications', parent: 'azure-shop-notifications', provider: 'azure', region: 'westeurope',
    runtime: 'node 20', memoryMb: 1536, arch: '', triggers: ['timer'], service: 'shop-notifications', integration: AZ_INT,
    rpm: 0, errorRate: 0, avgMs: 0, p95Ms: 0, coldRate: 0, idle: true },
  { slug: 'azure-invoice-pdf', name: 'invoice-pdf', app: 'invoice-pdf', provider: 'azure', region: 'westeurope', runtime: 'dotnet-isolated 8', memoryMb: 2048,
    arch: '', triggers: ['http', 'queue:invoices'], service: 'invoice-pdf', integration: AZ_INT,
    functions: [{ name: 'RenderInvoice', trigger: 'queue:invoices' }, { name: 'RetryFailed', trigger: 'http' }], rpm: 85, errorRate: 0.024, avgMs: 1900, p95Ms: 5200, coldRate: 0.09, spikeAt: 0.55 },
];

function mockCost(m: MockFn, invocations: number): number {
  if (m.parent) return 0; // per-function rows carry no execution units: cost stays on the app row
  const gbs = (invocations * m.avgMs) / 1000 * (m.memoryMb / 1024);
  if (m.provider === 'aws') return invocations * 0.2 / 1e6 + gbs * (m.arch === 'arm64' ? 0.0000133334 : 0.0000166667);
  const units = invocations * m.avgMs * m.memoryMb; // MB·ms
  return units / 1024 / 1000 * 0.000016 + invocations * 0.2 / 1e6;
}

function mockSeries(m: MockFn, range: TimeRange) {
  const stepMin = RANGE_POINTS[range].stepMs / 60_000;
  const shape = m.spikeAt !== undefined ? { spikeAt: m.spikeAt, spikeWidth: 3 } : {};
  const n = RANGE_POINTS[range].points;
  if (m.idle) {
    const z = new Array<number>(n).fill(0);
    return { inv: z, err: z, thr: z, avg: z, p95: z, p99: z, conc: z, cold: z };
  }
  const inv = series(`faas-inv-${m.slug}`, range, { base: m.rpm * stepMin, noise: 0.12, daily: 0.4, ...shape, spikeMul: 1.6 }).map(Math.round);
  const err = series(`faas-err-${m.slug}`, range, { base: m.errorRate, noise: 0.4, ...shape, spikeMul: 3 }).map((r, i) => Math.round(r * inv[i]));
  const thr = m.throttlePerMin
    ? series(`faas-thr-${m.slug}`, range, { base: m.throttlePerMin * stepMin * 0.08, noise: 0.6, spikeAt: m.spikeAt ?? 0.8, spikeMul: 14, spikeWidth: 4 }).map(Math.round)
    : new Array<number>(n).fill(0);
  const avg = series(`faas-avg-${m.slug}`, range, { base: m.avgMs, noise: 0.08, ...shape, spikeMul: 1.5 });
  // the platform stops an invocation at its timeout, so no percentile goes above it
  const cap = m.timeoutS ? m.timeoutS * 1000 : Number.POSITIVE_INFINITY;
  const p95 = avg.map((v) => Math.min(cap, v * (m.p95Ms / m.avgMs)));
  const p99 = p95.map((v, i) => Math.min(cap, v * (1.35 + ((i * 7) % 5) * 0.03)));
  // concurrency ≈ arrival rate × duration (Little's law), capped at the account limit for the throttling one
  const conc = inv.map((v, i) => Math.min(m.throttlePerMin ? 200 : 1e6, Math.round((v / stepMin / 60) * (avg[i] / 1000) * 1.4)));
  const cold = inv.map((v, i) => Math.round(v * m.coldRate * (thr[i] > 0 ? 2.2 : 1)));
  return { inv, err, thr, avg, p95, p99, conc, cold };
}

const sum = (a: number[]) => a.reduce((x, v) => x + v, 0);
function mockRow(m: MockFn, range: TimeRange): FaasFunction {
  const s = mockSeries(m, range);
  const invocations = sum(s.inv);
  const errors = sum(s.err);
  const throttles = sum(s.thr);
  const coldStarts = sum(s.cold);
  const row: Omit<FaasFunction, 'status'> = {
    id: m.slug, slug: m.slug, name: m.name, provider: m.provider, app: m.app, region: m.region, runtime: m.runtime, memoryMb: m.memoryMb,
    timeoutS: m.timeoutS, arch: m.arch, triggers: m.triggers, service: m.service,
    invocations, errors, errorRate: invocations ? errors / invocations : 0, throttles,
    p95Ms: invocations ? Math.max(...s.p95.slice(-5)) : 0, p99Ms: invocations ? Math.max(...s.p99.slice(-5)) : 0,
    coldStarts, coldStartRate: invocations ? coldStarts / invocations : 0, concurrencyMax: Math.max(0, ...s.conc),
    costUsd: mockCost(m, invocations), spark: s.inv, parentId: m.parent, appInsights: m.appInsights,
  };
  return { ...row, status: faasStatus(row) };
}

const MOCK_ERRORS = [
  'Task timed out after {t} seconds', 'TypeError: Cannot read properties of undefined (reading \'sku\')', 'ConditionalCheckFailedException: The conditional request failed',
  'ProvisionedThroughputExceededException: Rate exceeded', 'System.TimeoutException: The operation has timed out', 'Runtime exited with error: signal: killed (OOM)',
];
function mockDetail(m: MockFn, range: TimeRange): FaasDetail {
  const s = mockSeries(m, range);
  const ts = timestamps(range);
  const set = (rows: [string, string, number[]][]): SeriesSet => ({ timestamps: ts, series: rows.map(([id, label, data]) => ({ id, label, data })) });
  const fn = mockRow(m, range);
  const errs = fn.errors === 0 ? [] : Array.from({ length: Math.min(8, fn.errors) }, (_, i) => ({
    ts: NOW - (i * 7 + 2) * 60_000 - (i * 13_000),
    traceId: (`${m.slug.length.toString(16)}${(i * 2654435761 >>> 0).toString(16)}f3a9c1d2e4b5a6c7d8e9f0a1b2c3d4e5`).slice(0, 32),
    message: MOCK_ERRORS[(i + m.name.length) % MOCK_ERRORS.length].replace('{t}', String(m.timeoutS ?? 230)),
  }));
  return {
    fn,
    invocations: set([['invocations', 'Invocations', s.inv], ['errors', 'Errors', s.err], ['throttles', 'Throttles', s.thr]]),
    duration: set([['avg', 'Average', s.avg], ['p95', 'p95', s.p95], ['p99', 'p99', s.p99]]),
    concurrency: set([['concurrency', 'Concurrent executions', s.conc]]),
    coldStarts: set([['coldStarts', 'Cold starts', s.cold]]),
    recentErrors: errs,
    functions: m.functions,
    children: m.provider === 'azure' && !m.parent ? MOCK_FNS.filter((x) => x.parent === m.slug).map((x) => mockRow(x, range)) : undefined,
    integration: m.integration,
  };
}

/**
 * Summary rule (contracts §22, api README): Azure per-function rows are left out of the metric sums (the app row carries
 * the app-level totals) and an app with per-function rows counts as its functions, not as one more.
 */
export function summaryRows(rows: FaasFunction[]): { countFn: boolean[]; addMetrics: boolean[] } {
  const ids = new Set(rows.map((r) => r.id));
  const hasChild = new Set(rows.filter((r) => r.parentId && ids.has(r.parentId)).map((r) => r.parentId!));
  return { countFn: rows.map((r) => !hasChild.has(r.id)), addMetrics: rows.map((r) => !r.parentId) };
}

function mockSummary(range: TimeRange): FaasSummary {
  const all = MOCK_FNS.map((m) => mockRow(m, range));
  const { countFn, addMetrics } = summaryRows(all);
  const rows = all.filter((_, i) => addMetrics[i]);
  const counted = all.filter((_, i) => countFn[i]);
  const invocations = sum(rows.map((r) => r.invocations));
  const errors = sum(rows.map((r) => r.errors));
  return {
    functions: counted.length, invocations, errors, errorRate: invocations ? errors / invocations : 0,
    throttles: sum(rows.map((r) => r.throttles)), coldStarts: sum(rows.map((r) => r.coldStarts)), costUsd: sum(rows.map((r) => r.costUsd)),
    byProvider: (['aws', 'azure'] as FaasProvider[]).map((p) => {
      const rs = rows.filter((r) => r.provider === p);
      return { provider: p, functions: counted.filter((r) => r.provider === p).length, invocations: sum(rs.map((r) => r.invocations)), costUsd: sum(rs.map((r) => r.costUsd)) };
    }),
  };
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
  if (!isApi) return mockSummary(range);
  return remote(`/serverless/summary${qs({ range })}`, EMPTY_SUMMARY, (r: RawSummary | null) => mapSummary(r));
}

/** `GET /serverless/functions?range=` — provider / search filtering happens client-side (one cached list per range). */
export function serverlessFunctions(range: TimeRange): FaasFunction[] {
  if (!isApi) return MOCK_FNS.map((m) => mockRow(m, range));
  return remote<FaasFunction[]>(`/serverless/functions${qs({ range })}`, [], (r: RawFn[] | null) => arr(r).map(mapFn));
}

/** `GET /serverless/functions/{id}?range=` — undefined while loading or when unknown. */
export function serverlessFunction(id: string, range: TimeRange): FaasDetail | undefined {
  if (!isApi) {
    const m = MOCK_FNS.find((x) => x.slug === id);
    return m ? mockDetail(m, range) : undefined;
  }
  return remote<FaasDetail | undefined>(`/serverless/functions/${seg(id)}${qs({ range })}`, undefined, (r: RawDetail | null) => (r ? mapDetail(r) : undefined));
}

/** Functions whose span `service` is this service (ServiceDetail link). */
export function functionsOfService(serviceId: string, range: TimeRange = '1h'): FaasFunction[] {
  return serverlessFunctions(range).filter((f) => f.service === serviceId);
}
