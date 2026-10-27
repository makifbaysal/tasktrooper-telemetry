/**
 * Api mode (ADR-013) for the operational lists exported from data/mock.ts: monitors, incidents,
 * load-test scenarios/runs, and the code-intelligence lists that have no endpoint yet.
 * Imported for its side effect (hydrator registration) by data/kpi.ts.
 */
import { NOW, incidents, inventory, monitors, repos, runs, scenarios, suggestions } from '../mock';
import { ApiError, api, hydrateList, invalidate, isApi, qs, registerHydrator } from '../remote';
import type { Incident, LoadTestRun, LoadTestScenario, Monitor, MonitorKind, QueueRule, Severity } from '../types';

// ---------------------------------------------------------------- monitors

export interface MonitorTargetSpec { service?: string; endpoint?: string; process?: string; view?: string; kpiId?: string }
/** `renotifyMin`: remind the channels every N minutes while firing (0/absent = once). */
export interface MonitorRule { op: '>' | '<' | '>=' | '<='; threshold: number; forMin: number; minRequests?: number; renotifyMin?: number }

/** What the api returns on top of the UI Monitor (services/api/openapi.yaml `Monitor`). */
export interface ApiMonitor extends Monitor {
  /** kind as stored (engine kinds like error_rate are mapped to the closest UI kind in `kind`) */
  apiKind?: string;
  targetSpec?: MonitorTargetSpec;
  rule?: MonitorRule;
  query?: Record<string, unknown>;
  /** active | muted | draft */
  status?: string;
  /** unix ms; muted while in the future */
  mutedUntil?: number;
  lastValue?: number | null;
}

interface RawMonitor {
  id: string; name: string; kind: string; target: string; condition: string; severity: string; state: string; source: string;
  lastChange: number; channels?: string[] | null; queue?: QueueRule | null; codeRef?: string | null; status?: string; targetSpec?: MonitorTargetSpec;
  query?: Record<string, unknown> | null; rule?: MonitorRule; lastValue?: number | null; mutedUntil?: number | string | null;
}

/** Engine kinds without a UI MonitorKind (`apiKind` keeps the real one). */
const UI_KIND: Record<string, MonitorKind> = {
  error_rate: 'metric_threshold', throughput: 'throughput_change', apdex: 'latency', log_count: 'log_match',
  log_pattern: 'log_pattern_new', dlq: 'consumer_lag', consumer_failures: 'consumer_lag', crash_rate: 'rum_vital',
};
const KINDS = new Set<string>(['metric_threshold', 'latency', 'throughput_change', 'anomaly', 'log_match', 'log_pattern_new', 'error_new', 'consumer_lag', 'job_missed', 'rum_vital', 'kpi_burn', 'composite']);
const sev = (s: string): Severity => (s === 'critical' || s === 'warning' ? s : 'info');
const toMs = (v: number | string | null | undefined) => (v == null || v === '' ? undefined : typeof v === 'number' ? v : Date.parse(v) || undefined);

export function mapMonitor(r: RawMonitor): ApiMonitor {
  const mutedUntil = toMs(r.mutedUntil);
  const muted = r.status === 'muted' || (mutedUntil !== undefined && mutedUntil > Date.now());
  const state: Monitor['state'] = muted ? 'muted' : r.state === 'firing' || r.state === 'pending' ? r.state : 'ok';
  return {
    id: r.id, name: r.name, kind: KINDS.has(r.kind) ? (r.kind as MonitorKind) : UI_KIND[r.kind] ?? 'metric_threshold', apiKind: r.kind,
    target: r.target, condition: r.condition, severity: sev(r.severity), state,
    source: r.source === 'code' || r.source === 'template' || r.source === 'suggestion' ? r.source : 'manual', codeRef: r.codeRef || undefined,
    lastChange: r.lastChange, channels: r.channels ?? [], queue: r.queue ?? undefined,
    targetSpec: r.targetSpec, rule: r.rule, query: r.query ?? undefined, status: r.status, mutedUntil, lastValue: r.lastValue,
  };
}

/** Body for POST/PUT /monitors (MonitorInput) from a monitor as the UI holds it. */
export function monitorInput(m: ApiMonitor, patch: Partial<{ status: string; name: string }> = {}) {
  return {
    name: patch.name ?? m.name, kind: m.apiKind ?? m.kind, target: m.targetSpec ?? {}, query: m.query ?? {},
    condition: m.rule ?? { op: '>', threshold: 0, forMin: 5 }, severity: m.severity, channels: m.channels,
    source: m.source, status: patch.status ?? (m.status === 'muted' ? 'muted' : 'active'), queue: m.queue ?? null,
  };
}

// ---------------------------------------------------------------- incidents

export interface ApiIncident extends Incident {
  endpoint?: string;
  ackedAt?: number;
  resolvedAt?: number;
  peakValue?: number | null;
  rootCause?: unknown;
  /** resolved straight from pending — never fired, nobody was notified */
  quiet?: boolean;
}

/** Incident timeline entry (GET /incidents/{id}/events). */
export interface IncidentEvent { id: string; ts: number; kind: 'pending' | 'fired' | 'notified' | 'acked' | 'resolved' | 'suppressed' | 'reminder' | string; data?: Record<string, unknown> | null }

interface RawIncident {
  id: string; monitorId?: string; kpiId?: string; title: string; severity: string; state: string; startedAt: number; value?: string;
  service?: string; endpoint?: string; ackedAt?: number; resolvedAt?: number; peakValue?: number | null; rootCause?: unknown; quiet?: boolean;
}

export function mapIncident(r: RawIncident): ApiIncident {
  const state: Incident['state'] = r.state === 'pending' || r.state === 'firing' || r.state === 'acknowledged' || r.state === 'resolved' ? r.state : 'firing';
  return {
    id: r.id, monitorId: r.monitorId ?? '', kpiId: r.kpiId || undefined, title: r.title, severity: sev(r.severity), state,
    startedAt: r.startedAt, value: r.value || (r.peakValue != null ? String(+r.peakValue.toPrecision(4)) : '—'),
    service: r.service || r.endpoint || '—', endpoint: r.endpoint || undefined,
    ackedAt: r.ackedAt || undefined, resolvedAt: r.resolvedAt || undefined, peakValue: r.peakValue, rootCause: r.rootCause ?? undefined, quiet: r.quiet || undefined,
  };
}

/** Resolved incidents of the last 7 days (api mode; the mock history lives in screens/Alerts.tsx). */
export const resolvedIncidents: (ApiIncident & { resolvedAt: number })[] = [];

// ---------------------------------------------------------------- maintenance windows

export interface MaintenanceWindow {
  id: string;
  name: string;
  startsAt: number;
  endsAt: number;
  /** empty = whole project */
  scope: { services?: string[]; monitorIds?: string[] };
}
export const maintenanceWindows: MaintenanceWindow[] = [];
/** false when the api has no /maintenance-windows yet (404) */
export let maintenanceAvailable = !isApi;
if (!isApi) {
  maintenanceWindows.push({ id: 'mw1', name: 'orders-db Postgres 16.6 minor upgrade', startsAt: NOW + 20 * 3600_000, endsAt: NOW + 21 * 3600_000, scope: { services: ['orders-api', 'order-events'] } });
}

interface RawWindow { id: string | number; name: string; startsAt: number | string; endsAt: number | string; scope?: MaintenanceWindow['scope'] | null }
const mapWindow = (w: RawWindow): MaintenanceWindow => ({
  id: String(w.id), name: w.name, startsAt: toMs(w.startsAt) ?? 0, endsAt: toMs(w.endsAt) ?? 0, scope: w.scope ?? {},
});

// ---------------------------------------------------------------- load tests

/**
 * The api assigns scenario / run ids, but the UI navigates to a new run right after starting it.
 * Started items keep the local id they were created with: `localId` maps server → local for
 * hydration, `serverId` maps back for api calls.
 */
const toLocal = new Map<string, string>();
const toServer = new Map<string, string>();
export const localId = (id: string) => toLocal.get(id) ?? id;
export const serverId = (id: string) => toServer.get(id) ?? id;
export function linkIds(local: string, server: string) { toLocal.set(server, local); toServer.set(local, server); }

/** Scenario `spec` JSON as stored by the api, by local id (lib/loadtest.ts reads it into SPEC). */
export const scenarioSpecs = new Map<string, Record<string, unknown>>();

interface RawScenario extends Omit<LoadTestScenario, 'source'> { source: string; spec?: Record<string, unknown> | null }
const mapScenario = (r: RawScenario): LoadTestScenario => {
  const id = localId(r.id);
  if (r.spec) scenarioSpecs.set(id, r.spec);
  return {
    id, name: r.name, target: r.target ?? '', source: r.source === 'generated' ? 'generated' : 'manual', steps: r.steps ?? 0,
    peakVus: r.peakVus ?? 0, duration: r.duration ?? 0, regions: r.regions ?? [], lastRun: r.lastRun ? localId(r.lastRun) : undefined,
  };
};
interface RawRun extends Omit<LoadTestRun, 'state'> { state: string; finishedAt?: number }
const RUN_STATES = new Set(['queued', 'running', 'passed', 'failed', 'aborted']);
export const mapRun = (r: RawRun): LoadTestRun => ({
  ...r, id: localId(r.id), scenarioId: localId(r.scenarioId),
  state: (RUN_STATES.has(r.state) ? r.state : r.state === 'error' ? 'failed' : 'queued') as LoadTestRun['state'], thresholds: r.thresholds ?? [],
});

// ---------------------------------------------------------------- POST-shaped reads (previews)

interface PEntry { value?: unknown; at: number; inflight?: boolean }
const pcache = new Map<string, PEntry>();
const PTTL = 60_000;
/** Bump the data version without dropping anything (remote.ts only exposes invalidate()). */
const notify = () => invalidate('\u0000');

/**
 * Like remote(), for read-only POST endpoints (/monitors/preview): cached by `key`, returns
 * `fallback` until the response lands, then re-renders useData() screens.
 */
export function postRemote<T>(key: string, path: string, body: unknown, fallback: T): T {
  const e = pcache.get(key);
  if (!e || (!e.inflight && Date.now() - e.at > PTTL)) {
    const n: PEntry = e ?? { at: 0 };
    n.inflight = true;
    pcache.set(key, n);
    api<T>(path, { method: 'POST', body: JSON.stringify(body) })
      .then((v) => { n.value = v; })
      .catch((err: unknown) => { console.warn('[data]', path, err instanceof Error ? err.message : err); })
      .finally(() => { n.inflight = false; n.at = Date.now(); notify(); });
  }
  return e?.value !== undefined ? (e.value as T) : fallback;
}

// ---------------------------------------------------------------- hydration

if (isApi) {
  // no api yet for code intelligence: show nothing rather than mock data
  suggestions.splice(0);
  repos.splice(0);
  inventory.splice(0);

  registerHydrator(() => hydrateList(monitors, '/monitors', (raw: RawMonitor[]) => raw.map(mapMonitor)));
  registerHydrator(() => hydrateList(incidents, `/incidents${qs({ state: 'open', limit: 500 })}`, (raw: RawIncident[]) => raw.map(mapIncident)));
  registerHydrator(() => hydrateList(resolvedIncidents, `/incidents${qs({ state: 'resolved', since: Date.now() - 7 * 86400_000, limit: 500 })}`,
    // quiet ones (cleared while pending, never notified) are not part of the history
    (raw: RawIncident[]) => raw.map(mapIncident).filter((i) => !i.quiet).map((i) => ({ ...i, resolvedAt: i.resolvedAt ?? i.startedAt }))));
  registerHydrator(() => hydrateList(scenarios, '/loadtests/scenarios', (raw: RawScenario[]) => raw.map(mapScenario)));
  registerHydrator(() => hydrateList(runs, '/loadtests/runs', (raw: RawRun[]) => raw.map(mapRun).sort((a, b) => b.startedAt - a.startedAt)));
  // optional endpoint: a 404 must not block startup
  registerHydrator(() => hydrateList(maintenanceWindows, '/maintenance-windows', (raw: RawWindow[]) => raw.map(mapWindow))
    .then(() => { maintenanceAvailable = true; })
    .catch((e: unknown) => { maintenanceAvailable = false; if (!(e instanceof ApiError && (e.status === 404 || e.status === 405))) console.warn('[data] /maintenance-windows', e); }));
}
