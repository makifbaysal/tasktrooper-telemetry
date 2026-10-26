// Per-service log lines. Every service (backend, worker, web, mobile) ships logs through its SDK,
// so the service page can show its own logs. Deterministic mock built from the service's type + endpoints.
import type { LogLine, Service, TimeRange } from './types';
import { DEGRADED_SINCE, NOW, logs as baseLogs, mockApm, series, services, timestamps } from './mock';
import { api, isApi, qs, registerHydrator, remote } from './remote';
import { level, mapLogPage, pick, seriesSet, type RawLogPage, type RawLogPattern } from './api/apm';

const endpointsFor = mockApm.endpointsFor;

type Tpl = [LogLine['level'], string, number?]; // level, message, weight

function rng(seed: number) {
  return () => {
    seed |= 0; seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
const hash = (s: string) => { let h = 7; for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0; return h; };

/** Incident-specific lines so a service's logs agree with its root cause. */
const INCIDENT: Record<string, Tpl[]> = {
  'storefront-bff': [
    ['warn', 'upstream slow service=recommendation-api route=/v1/recommendations duration_ms={ms}', 4],
    ['error', 'upstream timeout service=recommendation-api after 1500ms, rendering home without recommendations', 1],
  ],
  'recommendation-api': [
    ['warn', 'slow query db=reco-db fingerprint=8f3a91c2 duration_ms={ms} rows=2400', 5],
    ['error', 'pq: canceling statement due to statement timeout db=reco-db fingerprint=8f3a91c2', 1],
    ['warn', 'connection pool saturated db=reco-db in_use=50/50 wait_ms={ms}', 2],
  ],
  'shop-web': [
    ['warn', 'slow fetch GET /v1/home duration_ms={ms} page=/', 3],
  ],
};

function templates(s: Service): Tpl[] {
  const out: Tpl[] = [];
  if (s.type === 'http') {
    for (const e of endpointsFor(s.id).slice(0, 6)) {
      out.push(['info', `${e.method} ${e.route} 200 {fast}ms`, 6]);
      out.push(['debug', `${e.method} ${e.route} cache lookup key=${e.route.split('/')[2] ?? 'root'}:{id}`, 1]);
      if (e.errorRate > 0.002) out.push(['error', `${e.method} ${e.route} 500 internal error request_id={id}`, 1]);
    }
    out.push(['warn', 'request slow route={route} duration_ms={ms}', 1]);
    out.push(['info', 'health check ok uptime_s={n}', 1]);
  } else if (s.type === 'worker' || s.type === 'consumer') {
    out.push(['info', 'job processed id={id} duration_ms={fast}', 6]);
    out.push(['warn', 'retrying attempt=2 id={id} reason=lock_timeout', 1]);
    out.push(['error', 'job failed id={id} moved to DLQ', 0.4]);
  } else if (s.type === 'web') {
    out.push(['info', 'route change to={route} duration_ms={fast}', 6]);
    out.push(['warn', 'slow fetch GET /v1/{seg} duration_ms={ms}', 1]);
    out.push(['error', "Uncaught TypeError: Cannot read properties of undefined (reading 'price') at ProductCard (ProductCard.tsx:42)", 0.6]);
    out.push(['error', 'ChunkLoadError: Loading chunk {n} failed', 0.3]);
    out.push(['debug', 'web-vitals LCP={ms} page={route}', 1]);
  } else {
    out.push(['info', 'screen_view name={screen} duration_ms={fast}', 6]);
    out.push(['warn', 'network request slow GET /v1/{seg} duration_ms={ms}', 1]);
    out.push(['error', 'network request failed GET /v1/{seg} code=-1009 offline=false', 0.5]);
    out.push(['error', 'ANR 5s on main thread in {screen}', 0.2]);
  }
  return [...out, ...(INCIDENT[s.id] ?? [])];
}

const ROUTES = ['/', '/p/:slug', '/cart', '/checkout', '/orders', '/search'];
const SCREENS = ['Home', 'ProductDetail', 'Cart', 'Checkout', 'OrderTracking'];
const SEGS = ['home', 'orders', 'items', 'cart', 'recommendations'];

function generate(s: Service): LogLine[] {
  const tpls = templates(s);
  const total = tpls.reduce((a, t) => a + (t[2] ?? 1), 0);
  const rnd = rng(hash(s.id));
  const incident = INCIDENT[s.id] ?? [];
  const routes = s.type === 'http' ? endpointsFor(s.id).map((e) => e.route) : ROUTES;
  return Array.from({ length: 90 }, (_, i) => {
    const ts = NOW - i * 3_900 - Math.round(rnd() * 2500);
    // incident lines only appear after the degradation started
    let pick = rnd() * total, t = tpls[0];
    for (const x of tpls) { pick -= x[2] ?? 1; if (pick <= 0) { t = x; break; } }
    if (incident.includes(t) && ts < DEGRADED_SINCE) t = tpls[0];
    const msg = t[1]
      .replace('{fast}', String(Math.round(8 + rnd() * 60)))
      .replace('{ms}', String(Math.round(600 + rnd() * 1600)))
      .replace('{id}', Math.floor(rnd() * 1e8).toString(36))
      .replace('{n}', String(Math.floor(rnd() * 900 + 10)))
      .replace('{route}', routes[Math.floor(rnd() * routes.length)])
      .replace('{screen}', SCREENS[Math.floor(rnd() * SCREENS.length)])
      .replace('{seg}', SEGS[Math.floor(rnd() * SEGS.length)]);
    return {
      id: `${s.id}-l${i}`, ts, level: t[0], service: s.name, message: msg,
      traceId: rnd() > 0.25 ? Math.floor(rnd() * 1e16).toString(16).padStart(16, '0') : undefined,
    };
  });
}

/** Every log line in the project, newest first. Api mode: the newest lines of the last hour, refreshed with the lists. */
export const allLogs: LogLine[] = isApi ? [] : [...baseLogs, ...services.flatMap(generate)].sort((a, b) => b.ts - a.ts);

const LIMIT = 500;
if (isApi) {
  // one request fills both exported line lists in place (`logs` is the short project-wide list in mock.ts)
  baseLogs.length = 0;
  registerHydrator(async () => {
    const lines = mapLogPage(await api<RawLogPage>(`/logs${qs({ range: '1h', limit: LIMIT })}`));
    allLogs.splice(0, allLogs.length, ...lines);
    baseLogs.splice(0, baseLogs.length, ...lines.slice(0, 120));
  });
}

/** Stable per-service arrays: screens memoize the returned array, so api responses are copied into it in place. */
const perService = new Map<string, LogLine[]>();

/** Log lines emitted by one service (looked up by service id). */
export function logsFor(serviceId: string): LogLine[] {
  const s = services.find((x) => x.id === serviceId);
  if (!isApi) return s ? allLogs.filter((l) => l.service === s.name) : [];
  const name = s?.name ?? serviceId;
  let out = perService.get(name);
  if (!out) {
    const arr = allLogs.filter((l) => l.service === name);
    out = arr;
    perService.set(name, arr);
  }
  const target = out;
  remote(`/logs${qs({ range: '1h', service: name, limit: LIMIT })}`, undefined, (raw: RawLogPage) => {
    const lines = mapLogPage(raw);
    target.splice(0, target.length, ...lines);
    return lines;
  });
  return out;
}

export interface LogQuery { serviceId?: string; levels?: LogLine['level'][]; q?: string; traceId?: string; route?: string; limit?: number }
const svcName = (id?: string) => (id ? services.find((x) => x.id === id)?.name ?? id : undefined);
const logQs = (range: TimeRange, f: LogQuery) =>
  qs({ range, service: svcName(f.serviceId), level: f.levels?.join(','), q: f.q, traceId: f.traceId, route: f.route });

/** Server-side log search (full-text `q`, levels, trace id). Mock: filters the generated lines. */
export function searchLogs(range: TimeRange, f: LogQuery = {}): LogLine[] {
  if (isApi) return remote(`/logs${logQs(range, f)}&limit=${f.limit ?? 200}`, [], mapLogPage);
  const base = f.serviceId ? logsFor(f.serviceId) : allLogs;
  return base.filter((l) => (!f.levels || f.levels.includes(l.level)) && (!f.traceId || l.traceId === f.traceId)
    && (!f.q || l.message.toLowerCase().includes(f.q.toLowerCase()))).slice(0, f.limit ?? 200);
}

/** Log volume per level over time (error / warn / info / debug). */
export function logVolumeSeries(range: TimeRange, f: LogQuery = {}): Record<LogLine['level'], number[]> {
  const n = timestamps(range).length;
  if (isApi) {
    const set = seriesSet(`/logs/series${logQs(range, f)}`, range);
    return { error: pick(set, 'error', n), warn: pick(set, 'warn', n), info: pick(set, 'info', n), debug: pick(set, 'debug', n) };
  }
  const seed = f.serviceId ?? 'all';
  return {
    info: series(`log-info-${seed}`, range, { base: 1800 }), warn: series(`log-warn-${seed}`, range, { base: 160, noise: 0.3 }),
    error: series(`log-err-${seed}`, range, { base: 60, noise: 0.4 }), debug: series(`log-debug-${seed}`, range, { base: 40 }),
  };
}

export interface LogPattern { id: string; pattern: string; level: LogLine['level']; services: string[]; count: number; last: number }
/** Drain-style log templates. Mock: masks numbers/ids of the generated lines. */
export function logPatterns(range: TimeRange, f: LogQuery = {}): LogPattern[] {
  if (isApi) {
    return remote(`/logs/patterns${logQs(range, f)}`, [], (raw: RawLogPattern[] | null) => (raw ?? []).map((p) => ({
      id: p.id, pattern: p.pattern, level: level(p.level),
      services: p.services ?? [], count: p.count, last: p.last,
    })));
  }
  const acc = new Map<string, LogPattern>();
  for (const l of searchLogs(range, { ...f, limit: 10_000 })) {
    const pattern = l.message.replace(/\b[0-9a-f]{8,}\b|\b\d+(\.\d+)?\b/gi, '<*>');
    const id = `${l.level}|${pattern}`;
    const cur = acc.get(id) ?? { id, pattern, level: l.level, services: [], count: 0, last: 0 };
    cur.count++;
    cur.last = Math.max(cur.last, l.ts);
    if (!cur.services.includes(l.service)) cur.services.push(l.service);
    acc.set(id, cur);
  }
  return [...acc.values()].sort((a, b) => b.count - a.count);
}
