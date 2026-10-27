/**
 * One trace with all of its spans (Trace detail screen, `/traces/:traceId`).
 *
 * Api mode: `GET /traces/{traceId}` (services/api/openapi.yaml `Trace`). Mock mode: a deterministic, realistic trace for
 * any id — browser → storefront-bff → the request's service → its database, cache, downstream services and a queue
 * producer, with exactly one failed span. Ids that appear in mock lists (endpoint and pod trace tables, log lines,
 * serverless recent errors) resolve to a trace matching that row: same service, route, status, duration and pod.
 */
import { NOW, endpointCalls, endpointsFor, logs as baseLogs, podTraces, services, tracesFor, type TraceSummary } from './mock';
import { allLogs } from './logs';
import { mockK8sPods } from './k8s';
import { serverlessFunction, serverlessFunctions } from './serverless';
import { isApi, remote, remoteStatus, seg } from './remote';
import type { Endpoint, K8sPod, TimeCategory } from './types';

// ---------------------------------------------------------------- types
export type SpanKind = 'server' | 'client' | 'producer' | 'consumer' | 'internal';
export interface TraceSpanEvent {
  name: string;
  /** ms from trace start */
  ts: number;
  attrs: Record<string, string>;
}
export interface TraceSpan {
  spanId: string;
  /** '' for a root span */
  parentSpanId: string;
  service: string;
  name: string;
  kind: SpanKind;
  category: TimeCategory;
  /** ms from trace start */
  start: number;
  /** ms */
  dur: number;
  /** HTTP / gRPC status code, 0 when none */
  status: number;
  failed: boolean;
  /** client/producer spans: "postgres · orders-db", "HTTP · payments-api", "kafka · orders.created" */
  dep?: string;
  /** entry spans: route template */
  route?: string;
  errorType?: string;
  attrs: Record<string, string>;
  /** resource attributes: k8s.pod.name, k8s.namespace.name, k8s.node.name, host.name, service.version … */
  resource: Record<string, string>;
  events: TraceSpanEvent[];
}
export interface TraceDetail {
  id: string;
  /** epoch ms of the first span */
  ts: number;
  /** ms */
  dur: number;
  services: string[];
  errors: number;
  spans: TraceSpan[];
}
export interface TraceState { trace?: TraceDetail; loading: boolean; error?: string; notFound?: boolean }

// ---------------------------------------------------------------- api mapping
interface RawSpan {
  spanId: string; parentSpanId?: string | null; service: string; name: string; kind: string; category: string; start: number; dur: number;
  status: number; failed: boolean; dep?: string | null; route?: string | null; errorType?: string | null;
  attrs?: Record<string, string> | null; resource?: Record<string, string> | null;
  events?: { name: string; ts: number; attrs?: Record<string, string> | null }[] | null;
}
interface RawTrace { id: string; ts: number; dur: number; services: string[] | null; errors: number; spans: RawSpan[] | null }

const KINDS: SpanKind[] = ['server', 'client', 'producer', 'consumer', 'internal'];
const CATS: TimeCategory[] = ['app', 'db', 'cache', 'http', 'queue'];
const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
/** Resource keys that older api versions merged into span attrs. */
const RESOURCE_PREFIXES = ['k8s.', 'host.', 'container.', 'cloud.', 'process.', 'os.', 'telemetry.sdk.', 'service.version', 'service.instance.id', 'deployment.environment', 'faas.'];
export const isResourceKey = (k: string) => RESOURCE_PREFIXES.some((p) => k.startsWith(p));

export function mapTrace(r: RawTrace): TraceDetail {
  const spans = (r.spans ?? []).map((s): TraceSpan => {
    const attrs: Record<string, string> = {};
    const resource: Record<string, string> = { ...(s.resource ?? {}) };
    for (const [k, v] of Object.entries(s.attrs ?? {})) {
      if (!s.resource && isResourceKey(k)) resource[k] = v;
      else attrs[k] = v;
    }
    return {
      spanId: s.spanId, parentSpanId: s.parentSpanId && /[^0]/.test(s.parentSpanId) ? s.parentSpanId : '', service: s.service || 'unknown',
      name: s.name || '(unnamed)', kind: KINDS.includes(s.kind as SpanKind) ? (s.kind as SpanKind) : 'internal',
      category: CATS.includes(s.category as TimeCategory) ? (s.category as TimeCategory) : 'app',
      start: num(s.start), dur: num(s.dur), status: num(s.status), failed: !!s.failed,
      ...(s.dep ? { dep: s.dep } : {}), ...(s.route ? { route: s.route } : {}), ...(s.errorType ? { errorType: s.errorType } : {}),
      attrs, resource, events: (s.events ?? []).map((e) => ({ name: e.name, ts: num(e.ts), attrs: e.attrs ?? {} })),
    };
  });
  return {
    id: r.id, ts: num(r.ts), dur: num(r.dur), services: r.services ?? [...new Set(spans.map((s) => s.service))],
    errors: num(r.errors) || spans.filter((s) => s.failed).length, spans,
  };
}

/** The trace, or loading / not-found / error state. */
export function traceDetail(traceId: string): TraceState {
  if (!isApi) return { trace: mockTrace(traceId), loading: false };
  const path = `/traces/${seg(traceId)}`;
  const trace = remote<TraceDetail | undefined>(path, undefined, (raw: RawTrace) => mapTrace(raw));
  if (trace) return { trace, loading: false };
  const st = remoteStatus();
  const err = st.errors.find((e) => e.path === path)?.error;
  if (err) return { loading: false, error: err, notFound: /not found/i.test(err) };
  return { loading: true };
}

// ---------------------------------------------------------------- mock: seeds from mock lists
interface Seed {
  ts: number; dur: number; status: number; isError: boolean; service: string; method: string; route: string;
  pod?: Pick<K8sPod, 'name' | 'namespace' | 'cluster' | 'node'>; message?: string;
}

function rng(seed: number) {
  return () => {
    seed |= 0; seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
function hash(s: string): number {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 16777619);
  return h >>> 0;
}
const hex = (n: number, len: number) => n.toString(16).padStart(len, '0').slice(-len);

let index: Map<string, Seed> | undefined;
/** Every trace id shown by a mock list → the row it came from. Built once, lazily. */
function seedIndex(): Map<string, Seed> {
  if (index) return index;
  const m = new Map<string, Seed>();
  const add = (t: TraceSummary, pod?: Seed['pod']) => {
    if (!m.has(t.id)) m.set(t.id, { ts: t.ts, dur: t.dur, status: t.status, isError: t.isError || t.status >= 500, service: t.service, method: t.method, route: t.route, pod });
  };
  for (const s of services) {
    for (const ep of [undefined, ...endpointsFor(s.id)]) for (const t of tracesFor(s.id, ep, '1h', { limit: 6 })) add(t);
  }
  for (const p of mockK8sPods()) {
    if (!p.service) continue;
    for (const t of podTraces({ name: p.name, namespace: p.namespace, service: p.service }, '1h', { limit: 20 })) add(t, p);
  }
  for (const f of serverlessFunctions('1h')) {
    for (const e of serverlessFunction(f.slug, '1h')?.recentErrors ?? []) {
      if (!m.has(e.traceId)) m.set(e.traceId, { ts: e.ts, dur: (f.p95Ms || 800) * 1.4, status: 500, isError: true, service: f.service || f.name, method: 'POST', route: `/${f.name}`, message: e.message });
    }
  }
  for (const l of [...baseLogs, ...allLogs]) {
    if (!l.traceId || m.has(l.traceId)) continue;
    const svc = services.find((s) => s.name === l.service);
    const eps = svc ? endpointsFor(svc.id).filter((e) => !e.sub) : [];
    const ep = eps.length ? eps[hash(l.traceId) % eps.length] : undefined;
    const isError = l.level === 'error';
    m.set(l.traceId, {
      ts: l.ts - 40, dur: (ep?.p95 ?? svc?.p95 ?? 120) * (isError ? 1.3 : 0.6), status: isError ? 500 : 200, isError,
      service: l.service, method: ep?.method ?? 'GET', route: ep?.route ?? '/', message: isError ? l.message : undefined,
    });
  }
  index = m;
  return m;
}

function fallbackSeed(id: string): Seed {
  const h = hash(id);
  const http = services.filter((s) => s.type === 'http' && s.id !== 'storefront-bff');
  const s = http[h % Math.max(1, http.length)];
  const eps = s ? endpointsFor(s.id).filter((e) => !e.sub) : [];
  const ep = eps[(h >>> 8) % Math.max(1, eps.length)];
  const isError = h % 4 === 0;
  return {
    ts: NOW - (h % 3_600_000), dur: (ep?.p95 ?? s?.p95 ?? 150) * (0.5 + ((h >>> 4) % 10) / 10), status: isError ? 502 : 200, isError,
    service: s?.name ?? 'orders-api', method: ep?.method ?? 'GET', route: ep?.route ?? '/',
  };
}

// ---------------------------------------------------------------- mock: trace builder
interface Node {
  service: string; name: string; kind: SpanKind; category: TimeCategory; weight: number;
  attrs?: Record<string, string>; dep?: string; route?: string; status?: number;
  failed?: boolean; errorType?: string; errorMessage?: string; events?: { name: string; at: number; attrs: Record<string, string> }[];
  children?: Node[]; parallel?: boolean;
}

const svcByName = (name: string) => services.find((s) => s.name === name || s.id === name);

const DB_OPS = [
  (t: string) => [`SELECT ${t}`, `SELECT * FROM ${t} WHERE id = $1`],
  (t: string) => [`UPDATE ${t}`, `UPDATE ${t} SET status = $1, updated_at = now() WHERE id = $2`],
  (t: string) => [`INSERT ${t}`, `INSERT INTO ${t} (id, payload, created_at) VALUES ($1, $2, now())`],
];
function dbNode(service: string, dep: string, weight: number, r: () => number, op = 0): Node {
  const [system, db] = dep.split(' · ');
  const table = (db ?? 'app').replace(/-db$/, '').replace(/-/g, '_');
  const [name, stmt] = DB_OPS[op % DB_OPS.length](table);
  return {
    service, name, kind: 'client', category: 'db', weight, dep,
    attrs: { 'db.system': system ?? 'postgresql', 'db.name': db ?? 'app', 'db.statement': stmt, 'db.operation': name.split(' ')[0], 'server.port': system === 'mysql' ? '3306' : '5432', 'db.rows_affected': String(1 + Math.floor(r() * 3)) },
  };
}
function cacheNode(service: string, dep: string, key: string, hit: boolean): Node {
  return { service, name: `GET ${key}`, kind: 'client', category: 'cache', weight: 0.8, dep, attrs: { 'db.system': 'redis', 'db.operation': 'GET', 'db.statement': `GET ${key}`, 'cache.hit': String(hit) } };
}
function producerNode(service: string, dep: string, key: string): Node {
  const [system, topic] = dep.split(' · ');
  return {
    service, name: `${topic} publish`, kind: 'producer', category: 'queue', weight: 3, dep,
    attrs: { 'messaging.system': system, 'messaging.destination.name': topic, 'messaging.operation': 'publish', 'messaging.message.id': key, 'messaging.kafka.partition': String(hash(key) % 12) },
  };
}
function httpClient(service: string, target: string, method: string, path: string, weight: number, child?: Node): Node {
  return {
    service, name: `${method} ${target}`, kind: 'client', category: 'http', weight, dep: `HTTP · ${target}`, status: 200,
    attrs: { 'http.request.method': method, 'url.full': `http://${target}${path}`, 'server.address': target, 'http.response.status_code': '200' },
    ...(child ? { children: [child] } : {}),
  };
}
function serverNode(service: string, method: string, route: string, weight: number, children: Node[], r: () => number): Node {
  return {
    service, name: `${method} ${route}`, kind: 'server', category: 'app', weight, route, status: 200, children,
    attrs: { 'http.request.method': method, 'http.route': route, 'url.path': route.replace(/:(\w+)/g, () => String(1000 + Math.floor(r() * 9000))), 'http.response.status_code': '200', 'user_agent.original': 'storefront-bff/1.9 (node)' },
  };
}

/** The request's own work: the endpoint's dependencies (mock topology), always with a DB call and a queue producer. */
function serviceWork(seed: Seed, r: () => number, depth: number): Node[] {
  const svc = svcByName(seed.service);
  const ep: Endpoint | undefined = svc ? endpointsFor(svc.id).find((e) => e.method === seed.method && e.route === seed.route) ?? endpointsFor(svc.id)[0] : undefined;
  const calls = ep ? endpointCalls(ep) : [];
  const out: Node[] = [];
  const base = seed.service.replace(/-api$|-consumer$|-worker$/, '');
  let hasDb = false, hasQueue = false;
  for (const c of calls) {
    const n = Math.max(1, Math.min(4, Math.round(c.callsPerReq)));
    for (let i = 0; i < n; i++) {
      const w = Math.max(0.4, c.msPerReq / Math.max(1, c.callsPerReq));
      if (c.dep.kind === 'db') { out.push(dbNode(seed.service, c.dep.name, w, r, i)); hasDb = true; }
      else if (c.dep.kind === 'cache') out.push(cacheNode(seed.service, c.dep.name, `${base}:${Math.floor(r() * 1e5)}`, r() > 0.2));
      else if (c.dep.kind === 'queue') { if (!hasQueue) out.push(producerNode(seed.service, c.dep.name, `${base}_${hex(hash(seed.route + i), 8)}`)); hasQueue = true; }
      else {
        const target = c.dep.name.replace(/^HTTP · /, '');
        const ts = svcByName(target);
        // a known downstream service gets its own server span (one level deep, to keep traces readable)
        const child = ts && depth < 1 ? (() => {
          const tep = endpointsFor(ts.id).filter((e) => !e.sub)[Math.floor(r() * Math.max(1, endpointsFor(ts.id).filter((e) => !e.sub).length))];
          const sub: Seed = { ...seed, service: ts.name, method: tep?.method ?? 'POST', route: tep?.route ?? '/', isError: false };
          return serverNode(ts.name, sub.method, sub.route, w * 0.9, serviceWork(sub, r, depth + 1), r);
        })() : undefined;
        out.push(httpClient(seed.service, ts?.name ?? target, child ? (child.attrs!['http.request.method'] ?? 'POST') : 'POST', child?.route ?? '/v1/charge', w, child));
      }
      if (out.length > 9) break;
    }
  }
  if (!hasDb) out.unshift(dbNode(seed.service, `postgres · ${base}-db`, Math.max(1.5, seed.dur * 0.06), r));
  if (!hasQueue && depth === 0) out.push(producerNode(seed.service, `kafka · ${base}.events`, `${base}_${hex(hash(seed.route), 8)}`));
  return out;
}

const exceptionEvent = (type: string, message: string, stack: string, at = 1) =>
  ({ name: 'exception', at, attrs: { 'exception.type': type, 'exception.message': message, 'exception.stacktrace': stack } });

/** Mark exactly one span failed. A failed request propagates its status up the chain; otherwise the failure is a retried call. */
function injectError(root: Node, entry: Node, seed: Seed, r: () => number) {
  const status = seed.status >= 500 ? seed.status : 502;
  // deepest outgoing HTTP call of the entry service, else its first DB call
  const path: Node[] = [];
  const find = (n: Node, trail: Node[]): Node[] | undefined => {
    for (const c of n.children ?? []) {
      const t = [...trail, c];
      const deeper = find(c, t);
      if (deeper) return deeper;
      if (c.kind === 'client' && c.category === 'http' && !c.children?.length) return t;
    }
    return undefined;
  };
  const trail = find(entry, [entry]) ?? (() => { const d = entry.children?.find((c) => c.category === 'db'); return d ? [entry, d] : [entry]; })();
  path.push(...trail);
  const target = path[path.length - 1];
  const msg = seed.message ?? (target.category === 'db' ? 'deadlock detected' : `upstream ${target.attrs?.['server.address'] ?? 'dependency'} timed out after 5000 ms`);
  const type = target.category === 'db' ? 'PostgresError' : 'TimeoutError';
  const stack = `${type}: ${msg}\n    at ${target.service.replace(/-/g, '_')}.client.call (src/clients/http.ts:88:13)\n    at async handle (src/routes${seed.route.replace(/:\w+/g, '[id]')}.ts:42:7)\n    at async Router.dispatch (node_modules/@trooper/node/dist/http.js:118:9)`;
  if (seed.isError) {
    Object.assign(target, { failed: true, status: target.category === 'http' ? 504 : 0, errorType: type, errorMessage: msg, events: [exceptionEvent(type, msg, stack)] });
    if (target.attrs && target.category === 'http') target.attrs['http.response.status_code'] = '504';
    root.status = status;
    target.weight = Math.max(target.weight, seed.dur * 0.55);
  } else if (target.category === 'http') {
    // a 503 that the client retried successfully
    const parent = path[path.length - 2] ?? entry;
    const failedTry: Node = {
      ...target, children: undefined, weight: Math.max(2, target.weight * 0.4), failed: true, status: 503, errorType: 'HTTPError',
      attrs: { ...target.attrs, 'http.response.status_code': '503', 'http.resend_count': '0' },
      events: [exceptionEvent('HTTPError', '503 Service Unavailable', `HTTPError: 503 Service Unavailable\n    at fetchWithRetry (src/clients/http.ts:61:11)`), { name: 'retry', at: 1, attrs: { 'retry.attempt': '1', 'retry.backoff_ms': String(20 + Math.floor(r() * 80)) } }],
    };
    target.attrs = { ...target.attrs, 'http.resend_count': '1' };
    const i = parent.children!.indexOf(target);
    parent.children!.splice(i, 0, failedTry);
  } else {
    // a handled DB error (unique violation → update path)
    Object.assign(target, { failed: true, errorType: 'UniqueViolation', events: [exceptionEvent('UniqueViolation', 'duplicate key value violates unique constraint', 'PostgresError: duplicate key value violates unique constraint "pk"\n    at Pool.query (src/db.ts:31:9)')] });
  }
}

function resourceFor(service: string, seedPod: Seed['pod'], seedService: string): Record<string, string> {
  const svc = svcByName(service);
  const res: Record<string, string> = { 'service.name': service, 'deployment.environment': 'production' };
  if (svc?.version) res['service.version'] = svc.version;
  if (svc && (svc.type === 'web' || svc.type === 'mobile')) {
    res['telemetry.sdk.name'] = svc.type === 'web' ? '@trooper/browser' : '@trooper/react-native';
    if (svc.type === 'web') res['browser.platform'] = 'macOS';
    return res;
  }
  const pod = seedPod && service === seedService ? seedPod : (() => {
    const pods = mockK8sPods().filter((p) => (p.service === svc?.id || p.service === service) && p.node && p.phase === 'Running');
    return pods.length ? pods[hash(service) % pods.length] : undefined;
  })();
  if (pod) {
    Object.assign(res, { 'k8s.cluster.name': pod.cluster, 'k8s.namespace.name': pod.namespace, 'k8s.pod.name': pod.name, 'k8s.node.name': pod.node, 'host.name': pod.node });
  } else {
    res['host.name'] = `${service}-${hex(hash(service), 6)}`;
  }
  if (svc?.language) res['telemetry.sdk.language'] = svc.language.toLowerCase().replace(/\.js$/, 'js');
  return res;
}

function layout(n: Node, start: number, dur: number, parentId: string, seed: Seed, r: () => number, out: TraceSpan[], idOf: () => string) {
  const spanId = idOf();
  const span: TraceSpan = {
    spanId, parentSpanId: parentId, service: n.service, name: n.name, kind: n.kind, category: n.category, start, dur,
    status: n.status ?? 0, failed: !!n.failed, ...(n.dep ? { dep: n.dep } : {}), ...(n.route ? { route: n.route } : {}),
    ...(n.errorType ? { errorType: n.errorType } : {}),
    attrs: { ...n.attrs }, resource: resourceFor(n.service, seed.pod, seed.service),
    events: (n.events ?? []).map((e) => ({ name: e.name, ts: start + dur * e.at, attrs: e.attrs })),
  };
  out.push(span);
  const kids = n.children ?? [];
  if (!kids.length) return;
  const total = kids.reduce((a, c) => a + c.weight, 0);
  // children fill at most 88% of the parent; the rest is the parent's own work, spread as gaps
  const scale = Math.min(1, (dur * 0.88) / (total || 1));
  const gap = (dur - total * scale) / (kids.length + 1);
  let t = start + gap;
  for (const c of kids) {
    const d = Math.max(0.05, c.weight * scale * (0.9 + r() * 0.2));
    layout(c, t, Math.min(d, start + dur - t), spanId, seed, r, out, idOf);
    t += c.weight * scale + gap;
  }
}

/** A large batch trace (mock only: ids starting with `0000`) to exercise the rendering cap. */
function bulkChildren(seed: Seed, r: () => number): Node[] {
  return Array.from({ length: 300 }, (_, i) => ({
    service: seed.service, name: `process item ${i + 1}`, kind: 'internal' as const, category: 'app' as const, weight: 3,
    children: [dbNode(seed.service, 'postgres · orders-db', 1, r, i), cacheNode(seed.service, 'redis · cache-main', `item:${i}`, true),
      dbNode(seed.service, 'postgres · orders-db', 1, r, i + 1), ...Array.from({ length: 6 }, (_, j) => dbNode(seed.service, 'postgres · orders-db', 0.3, r, j))],
  }));
}

const mockCache = new Map<string, TraceDetail>();
function mockTrace(id: string): TraceDetail {
  const hit = mockCache.get(id);
  if (hit) return hit;
  const seed = seedIndex().get(id) ?? fallbackSeed(id);
  const r = rng(hash(id));
  const bff = 'storefront-bff';
  const client = hash(id) % 3 === 0 ? 'shop-mobile' : 'shop-web';
  const isBff = seed.service === bff;
  const isClient = svcByName(seed.service)?.type === 'web' || svcByName(seed.service)?.type === 'mobile';
  const entrySeed: Seed = isClient ? { ...seed, service: 'orders-api', method: 'POST', route: '/v1/orders' } : seed;
  const work = id.startsWith('0000') ? bulkChildren(entrySeed, r) : serviceWork(entrySeed, r, 0);
  const entry = serverNode(entrySeed.service, entrySeed.method, entrySeed.route, entrySeed.dur * 0.8, work, r);
  const apiPath = `/api${entrySeed.route.replace(/^\/v\d+/, '')}`;
  const bffServer: Node = isBff ? entry : serverNode(bff, entrySeed.method, apiPath, entrySeed.dur * 0.9, [
    cacheNode(bff, 'redis · cache-bff', `session:${hex(hash(id + 's'), 10)}`, true),
    httpClient(bff, 'auth-api', 'POST', '/v1/token/verify', Math.max(2, entrySeed.dur * 0.05),
      serverNode('auth-api', 'POST', '/v1/token/verify', Math.max(1.5, entrySeed.dur * 0.045), [cacheNode('auth-api', 'redis · cache-main', `token:${hex(hash(id + 't'), 12)}`, true)], r)),
    httpClient(bff, entrySeed.service, entrySeed.method, entrySeed.route, entrySeed.dur * 0.82, entry),
  ], r);
  if (bffServer.attrs) bffServer.attrs['user_agent.original'] = client === 'shop-web' ? 'Mozilla/5.0 (Macintosh; Intel Mac OS X 14_6) Chrome/130.0' : 'shop-mobile/4.12.0 (iOS 18.1)';
  const root: Node = {
    service: client, name: `${entrySeed.method} ${apiPath}`, kind: 'client', category: 'http', weight: seed.dur, status: 200, dep: `HTTP · ${bff}`,
    attrs: { 'http.request.method': entrySeed.method, 'url.full': `https://shop.example.com${apiPath}`, 'http.response.status_code': '200', ...(client === 'shop-web' ? { 'page.route': '/checkout', 'session.id': hex(hash(id + 'sess'), 16) } : { 'screen.name': 'CheckoutScreen' }) },
    children: [bffServer],
  };
  injectError(root, entry, seed, r);
  const spans: TraceSpan[] = [];
  let n = 0;
  const idOf = () => hex(hash(`${id}-${n++}`), 8) + hex(hash(`${n}-${id}`), 8);
  layout(root, 0, Math.max(seed.dur, 1), '', seed, r, spans, idOf);
  if (seed.isError) {
    // the failure surfaces as a 5xx on every HTTP span above it; only the failing call itself is an error span
    const byId = new Map(spans.map((s) => [s.spanId, s]));
    let p = byId.get(spans.find((s) => s.failed)?.parentSpanId ?? '');
    while (p) {
      if (p.attrs['http.response.status_code']) { p.status = root.status ?? 502; p.attrs['http.response.status_code'] = String(p.status); }
      p = byId.get(p.parentSpanId);
    }
  }
  const t: TraceDetail = {
    id, ts: seed.ts, dur: Math.max(seed.dur, 1), services: [...new Set(spans.map((s) => s.service))].sort(), errors: spans.filter((s) => s.failed).length, spans,
  };
  mockCache.set(id, t);
  return t;
}
