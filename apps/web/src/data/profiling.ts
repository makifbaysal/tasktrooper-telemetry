/**
 * Continuous profiling (ADR-022, .ai/contracts.md §24).
 *
 * Api mode: `GET /profiles/types | flamegraph | diff | top | series`. Mock mode: Go services (net/http + chi, encoding/json,
 * database/sql + pgx, go-redis, kafka-go, OTel) and Node services (express, pg, ioredis, kafkajs, pino) get stacks derived
 * from their real mock endpoints and dependencies, so flame graphs agree with the "Where time goes" breakdowns. The newest
 * version carries a regression (a regexp compiled per request in Go, a deep clone in Node) and a small win (less logging),
 * so version and time-window diffs have something to show. Python services have no profiler yet (empty state).
 */
import { NOW, RANGE_POINTS, endpointBreakdown, endpointCalls, endpointsFor, series as mockSeries, services, timestamps } from './mock';
import { deploysOf } from './integrations';
import { isApi, qs, remote, remoteStatus } from './remote';
import type { Endpoint, Service, TimeRange } from './types';

// ---------------------------------------------------------------- types (contracts §24)
export type ProfileType = 'cpu' | 'wall' | 'alloc_space' | 'alloc_objects' | 'inuse_space' | 'inuse_objects' | 'goroutines' | 'lock' | 'block';
export type ProfileUnit = 'nanoseconds' | 'bytes' | 'count';
export type ProfileRuntime = 'go' | 'jvm' | 'node' | 'dotnet' | 'python';

export const PROFILE_TYPES: { id: ProfileType; label: string; hint: string }[] = [
  { id: 'cpu', label: 'CPU', hint: 'On-CPU time' },
  { id: 'wall', label: 'Wall', hint: 'Wall-clock time, including waiting' },
  { id: 'alloc_space', label: 'Allocations', hint: 'Bytes allocated' },
  { id: 'alloc_objects', label: 'Allocated objects', hint: 'Objects allocated' },
  { id: 'inuse_space', label: 'Heap in use', hint: 'Live heap bytes, avg per snapshot' },
  { id: 'inuse_objects', label: 'Heap objects', hint: 'Live objects, avg per snapshot' },
  { id: 'goroutines', label: 'Goroutines', hint: 'Goroutines, avg per snapshot' },
  { id: 'lock', label: 'Lock', hint: 'Time waiting on mutexes / monitors' },
  { id: 'block', label: 'Block', hint: 'Time blocked on channels and I/O' },
];
export const profileTypeLabel = (t: ProfileType) => PROFILE_TYPES.find((x) => x.id === t)?.label ?? t;
/** Snapshot types: values are summed over profiles by the api; the UI shows them per profile. */
export const isGaugeType = (t: ProfileType) => t === 'inuse_space' || t === 'inuse_objects' || t === 'goroutines';

export interface ProfileTypeInfo { type: ProfileType; unit: ProfileUnit; runtime: ProfileRuntime; hosts: string[]; versions: string[]; routes: number }
export interface ProfileFilter { env?: string; version?: string; host?: string; route?: string; method?: string }

export interface FlameNode { name: string; value: number; self: number; children: FlameNode[] }
export interface DiffNode { name: string; value: number; self: number; baseValue: number; baseSelf: number; children: DiffNode[] }
export interface FlameGraphData { type: ProfileType; unit: ProfileUnit; total: number; profiles: number; from: number; to: number; root: FlameNode }
export interface ProfileDiffData { type: ProfileType; unit: ProfileUnit; total: number; baseTotal: number; root: DiffNode }
export interface TopFunction { name: string; self: number; total: number }
export interface TopFunctions { unit: ProfileUnit; total: number; functions: TopFunction[] }
export interface ProfileSeries { timestamps: number[]; series: { id: string; label: string; data: number[] }[] }

/** Compare either two versions or two time windows (unix ms). */
export type ProfileComparison =
  | { kind: 'version'; baseVersion: string; version: string }
  | { kind: 'window'; baseFrom: number; baseTo: number; from: number; to: number };

export interface Loadable<T> { data?: T; loading: boolean; error?: string }

// ---------------------------------------------------------------- formatting
const trimN = (n: number, d: number) => Number(n.toFixed(d)).toString();
export function formatProfileValue(unit: ProfileUnit, v: number): string {
  const a = Math.abs(v);
  if (unit === 'nanoseconds') {
    if (a >= 3600e9) return `${trimN(v / 3600e9, 1)} h`;
    if (a >= 60e9) return `${trimN(v / 60e9, 1)} min`;
    if (a >= 1e9) return `${trimN(v / 1e9, a >= 10e9 ? 0 : 1)} s`;
    if (a >= 1e6) return `${trimN(v / 1e6, 0)} ms`;
    return `${trimN(v / 1e3, 0)} µs`;
  }
  if (unit === 'bytes') {
    if (a >= 1024 ** 4) return `${trimN(v / 1024 ** 4, 1)} TiB`;
    if (a >= 1024 ** 3) return `${trimN(v / 1024 ** 3, 1)} GiB`;
    if (a >= 1024 ** 2) return `${trimN(v / 1024 ** 2, 1)} MiB`;
    if (a >= 1024) return `${trimN(v / 1024, 0)} KiB`;
    return `${Math.round(v)} B`;
  }
  if (a >= 1e9) return `${trimN(v / 1e9, 1)}B`;
  if (a >= 1e6) return `${trimN(v / 1e6, 1)}M`;
  if (a >= 1e3) return `${trimN(v / 1e3, 1)}K`;
  return String(Math.round(v));
}
/** Unit of the over-time chart for a type (`/profiles/series`). */
export function seriesFormat(type: ProfileType): (v: number) => string {
  if (type === 'cpu' || type === 'wall') return (v) => `${trimN(v, v >= 10 ? 0 : 2)} cores`;
  if (type === 'alloc_space') return (v) => `${formatProfileValue('bytes', v)}/min`;
  if (type === 'inuse_space') return (v) => formatProfileValue('bytes', v);
  if (type === 'lock' || type === 'block') return (v) => `${formatProfileValue('nanoseconds', v)}/min`;
  return (v) => formatProfileValue('count', v);
}

// ---------------------------------------------------------------- tree helpers (shared by mock and UI)
/** Self and total per function. Total counts a function once per stack (recursion is not double counted). */
export function topFromTree(root: FlameNode, limit = 50): TopFunction[] {
  const self = new Map<string, number>();
  const total = new Map<string, number>();
  const walk = (n: FlameNode, onPath: Set<string>) => {
    if (n !== root && !n.name.startsWith('[')) {
      self.set(n.name, (self.get(n.name) ?? 0) + n.self);
      if (!onPath.has(n.name)) total.set(n.name, (total.get(n.name) ?? 0) + n.value);
    }
    const next = n === root || onPath.has(n.name) ? onPath : new Set(onPath).add(n.name);
    for (const c of n.children) walk(c, next);
  };
  walk(root, new Set());
  return [...total.entries()].map(([name, t]) => ({ name, self: self.get(name) ?? 0, total: t }))
    .sort((a, b) => b.self - a.self || b.total - a.total).slice(0, limit);
}

export interface DiffChange { name: string; share: number; baseShare: number; delta: number }
/** Functions whose share of self time changed the most between base and target. */
export function diffChanges(d: ProfileDiffData, limit = 15): DiffChange[] {
  const cur = new Map<string, number>();
  const base = new Map<string, number>();
  const walk = (n: DiffNode, isRoot: boolean) => {
    if (!isRoot && !n.name.startsWith('[')) {
      cur.set(n.name, (cur.get(n.name) ?? 0) + n.self);
      base.set(n.name, (base.get(n.name) ?? 0) + n.baseSelf);
    }
    for (const c of n.children) walk(c, false);
  };
  walk(d.root, true);
  return [...new Set([...cur.keys(), ...base.keys()])].map((name) => {
    const share = d.total ? (cur.get(name) ?? 0) / d.total : 0;
    const baseShare = d.baseTotal ? (base.get(name) ?? 0) / d.baseTotal : 0;
    return { name, share, baseShare, delta: share - baseShare };
  }).filter((c) => Math.abs(c.delta) >= 0.0005).sort((a, b) => Math.abs(b.delta) - Math.abs(a.delta)).slice(0, limit);
}

// ---------------------------------------------------------------- api
const svcName = (id: string) => services.find((s) => s.id === id)?.name ?? id;
const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
function mapFlame(n: FlameNode | null | undefined): FlameNode {
  return { name: n?.name ?? 'all', value: num(n?.value), self: num(n?.self), children: (n?.children ?? []).map(mapFlame) };
}
function mapDiff(n: DiffNode | null | undefined): DiffNode {
  return { name: n?.name ?? 'all', value: num(n?.value), self: num(n?.self), baseValue: num(n?.baseValue), baseSelf: num(n?.baseSelf), children: (n?.children ?? []).map(mapDiff) };
}
function state<T>(path: string, fallback: T | undefined, map: (raw: never) => T): Loadable<T> {
  const data = remote<T | undefined>(path, fallback, map);
  if (data !== undefined) return { data, loading: false };
  const err = remoteStatus().errors.find((e) => e.path === path)?.error;
  return err ? { loading: false, error: err } : { loading: true };
}
const filterQs = (f: ProfileFilter) => ({ env: f.env, version: f.version, host: f.host, route: f.route, method: f.method });

/** Profile types that have data for a service. Empty → the service doesn't send profiles (setup instructions). */
export function profileTypes(serviceId: string, range: TimeRange): Loadable<ProfileTypeInfo[]> {
  if (!isApi) return { data: mockTypes(serviceId), loading: false };
  return state(`/profiles/types${qs({ service: svcName(serviceId), range })}`, undefined,
    (r: { types?: ProfileTypeInfo[] | null } | null) => (r?.types ?? []).map((t) => ({ ...t, hosts: t.hosts ?? [], versions: t.versions ?? [], routes: num(t.routes) })));
}

export function flameGraph(serviceId: string, type: ProfileType, range: TimeRange, f: ProfileFilter = {}): Loadable<FlameGraphData> {
  if (!isApi) return { data: mockFlame(serviceId, type, range, f), loading: false };
  return state(`/profiles/flamegraph${qs({ service: svcName(serviceId), type, range, ...filterQs(f) })}`, undefined,
    (r: FlameGraphData) => ({ ...r, total: num(r.total), profiles: num(r.profiles), root: mapFlame(r.root) }));
}

/** `range` scopes version-vs-version mode (api default 24h); window mode uses its own from/to. */
export function profileDiff(serviceId: string, type: ProfileType, cmp: ProfileComparison, f: ProfileFilter = {}, range: TimeRange = '24h'): Loadable<ProfileDiffData> {
  if (!isApi) return { data: mockDiff(serviceId, type, cmp, f), loading: false };
  const sel = cmp.kind === 'version' ? { baseVersion: cmp.baseVersion, version: cmp.version, range } : { baseFrom: cmp.baseFrom, baseTo: cmp.baseTo, from: cmp.from, to: cmp.to };
  const { version: _v, ...rest } = filterQs(f);
  void _v;
  return state(`/profiles/diff${qs({ service: svcName(serviceId), type, ...rest, ...sel })}`, undefined,
    (r: ProfileDiffData) => ({ ...r, total: num(r.total), baseTotal: num(r.baseTotal), root: mapDiff(r.root) }));
}

export function topFunctions(serviceId: string, type: ProfileType, range: TimeRange, f: ProfileFilter = {}): Loadable<TopFunctions> {
  if (!isApi) {
    const fg = mockFlame(serviceId, type, range, f);
    return { data: { unit: fg.unit, total: fg.total, functions: topFromTree(fg.root) }, loading: false };
  }
  return state(`/profiles/top${qs({ service: svcName(serviceId), type, range, ...filterQs(f), limit: 50 })}`, undefined,
    (r: TopFunctions) => ({ unit: r.unit, total: num(r.total), functions: r.functions ?? [] }));
}

/** cpu/wall: avg cores per host; others: per-minute average per host (≤ 8 hosts, rest "Other"). */
export function profileSeries(serviceId: string, type: ProfileType, range: TimeRange, f: ProfileFilter = {}): Loadable<ProfileSeries> {
  if (!isApi) return { data: mockProfileSeries(serviceId, type, range, f), loading: false };
  return state(`/profiles/series${qs({ service: svcName(serviceId), type, range, env: f.env, version: f.version })}`, undefined,
    (r: ProfileSeries) => ({ timestamps: r.timestamps ?? [], series: (r.series ?? []).map((s) => ({ ...s, data: (s.data ?? []).map(num) })) }));
}

// =====================================================================================================
// Mock
// =====================================================================================================
function h32(s: string) {
  let x = 2166136261;
  for (let i = 0; i < s.length; i++) x = Math.imul(x ^ s.charCodeAt(i), 16777619);
  return x >>> 0;
}
const jitter = (key: string, spread = 0.2) => 1 - spread / 2 + ((h32(key) % 1000) / 1000) * spread;

const runtimeOf = (s: Service): ProfileRuntime | undefined =>
  s.language === 'Go' ? 'go' : s.language === 'Node.js' ? 'node' : undefined;
const isBackend = (s: Service) => s.type !== 'web' && s.type !== 'mobile';

const MOCK_TYPES: Record<'go' | 'node', [ProfileType, ProfileUnit][]> = {
  go: [['cpu', 'nanoseconds'], ['alloc_space', 'bytes'], ['inuse_space', 'bytes'], ['goroutines', 'count'], ['lock', 'nanoseconds']],
  node: [['cpu', 'nanoseconds'], ['inuse_space', 'bytes']],
};

function mockHosts(s: Service): string[] {
  const n = Math.max(1, Math.min(s.instances || 1, 6));
  return Array.from({ length: n }, (_, i) => `${s.name}-${(h32(s.name) % 0xfffff).toString(16)}-${(h32(`${s.name}${i}`) % 0xfffff).toString(36).slice(0, 5)}`);
}
const mockVersions = (s: Service) => {
  const v = [...new Set(deploysOf(s.id).filter((d) => d.state !== 'error' && d.state !== 'canceled').map((d) => d.version))];
  return v.length ? v : [s.version];
};

function mockTypes(serviceId: string): ProfileTypeInfo[] {
  const s = services.find((x) => x.id === serviceId);
  const rt = s && isBackend(s) ? runtimeOf(s) : undefined;
  if (!s || !rt || (rt !== 'go' && rt !== 'node')) return [];
  const routes = rt === 'go' ? endpointsFor(s.id).filter((e) => !e.sub).length : 0;
  return MOCK_TYPES[rt].map(([type, unit]) => ({ type, unit, runtime: rt, hosts: mockHosts(s), versions: mockVersions(s), routes }));
}

interface Stack { frames: string[]; value: number; route?: string; method?: string }

const pascal = (s: string) => s.replace(/[^a-z0-9]+(.)?/gi, (_, c: string | undefined) => (c ? c.toUpperCase() : '')).replace(/^./, (c) => c.toUpperCase());
/** Handler name from the route: POST /v1/charges → CreateCharges, GET /v1/items/:id → GetItem. */
function handlerName(e: Endpoint): string {
  if (e.sub) return `Handle${pascal(e.sub.process)}`;
  const parts = e.route.split('/').filter((p) => p && !/^v\d+$/.test(p));
  const byId = parts[parts.length - 1]?.startsWith(':');
  const noun = pascal(parts.filter((p) => !p.startsWith(':')).pop() ?? 'root');
  const verb = e.method === 'GET' ? (byId ? 'Get' : 'List') : e.method === 'POST' ? 'Create' : e.method === 'DELETE' ? 'Delete' : 'Update';
  return `${verb}${byId ? noun.replace(/s$/, '') : noun}`;
}
const moduleOf = (s: Service) => s.name.replace(/-(api|consumer|worker|bff)$/, '');

/** Weight of each stack for a type; `regress` = the newest version's share of the window. */
function goStacks(s: Service, type: ProfileType, regress: number): Stack[] {
  const mod = moduleOf(s);
  const out: Stack[] = [];
  const push = (frames: string[], value: number, e?: Endpoint) => value > 0 && out.push({ frames, value, route: e?.route, method: e?.method });
  const httpBase = ['runtime.goexit', 'net/http.(*conn).serve', 'net/http.serverHandler.ServeHTTP', 'github.com/go-chi/chi/v5.(*Mux).ServeHTTP',
    'github.com/go-chi/chi/v5.(*Mux).routeHTTP', 'github.com/tasktrooper/sdk-go/integrations/chi.Middleware.func1'];
  const mallocLeaf = ['runtime.mallocgc', 'runtime.(*mcache).nextFree', 'runtime.(*mcache).refill'];
  const eps = endpointsFor(s.id);
  const top = [...eps].sort((a, b) => b.rpm - a.rpm)[0];

  for (const e of eps) {
    const b = endpointBreakdown(e);
    const calls = endpointCalls(e);
    const fn = handlerName(e);
    const domain = (e.route.split('/').filter((p) => p && !/^v\d+$/.test(p) && !p.startsWith(':'))[0] ?? 'core').replace(/[^a-z]/gi, '');
    const h = [...httpBase, `${mod}/internal/api.(*Handler).${fn}`];
    const svcFn = `${mod}/internal/${domain}.(*Service).${fn.replace(/^(Get|List|Create|Update|Delete)/, '$1')}`;
    const rps = e.rpm / 60;
    const k = (x: number) => x * rps;
    const app = b.parts.app;
    const isPost = e.method !== 'GET';
    if (type === 'cpu') {
      push([...h, svcFn], k(app * 0.42), e);
      push([...h, svcFn, `${mod}/internal/${domain}.validate`], k(app * 0.06), e);
      push([...h, 'encoding/json.Marshal', 'encoding/json.(*encodeState).marshal', 'encoding/json.(*encodeState).reflectValue', 'encoding/json.structEncoder.encode'], k(app * 0.14), e);
      push([...h, 'encoding/json.Marshal', 'encoding/json.(*encodeState).marshal', ...mallocLeaf.slice(0, 1)], k(app * 0.04), e);
      if (isPost) push([...h, 'encoding/json.(*Decoder).Decode', 'encoding/json.(*decodeState).object', 'encoding/json.(*decodeState).literalStore'], k(app * 0.08), e);
      push([...h, 'log/slog.(*Logger).Info', 'log/slog.(*JSONHandler).Handle', 'log/slog.(*handleState).appendAttr'], k(app * 0.05 * (1 - regress * 0.6)), e);
      push([...h.slice(0, 6), 'go.opentelemetry.io/otel/sdk/trace.(*recordingSpan).End', 'go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).OnEnd'], k(app * 0.025), e);
      if (e === top) push([...h, svcFn, `${mod}/internal/${domain}.validate`, 'regexp.MustCompile', 'regexp.compile', 'regexp/syntax.Parse', 'regexp/syntax.(*parser).push'], k(app * 0.2 * regress), e);
      if (e === top) push([...h, svcFn, `${mod}/internal/${domain}.validate`, 'regexp.MustCompile', 'regexp.compile', 'regexp.compileOnePass'], k(app * 0.07 * regress), e);
      for (const c of calls) {
        const st = `${mod}/internal/store.(*Store).${fn}`;
        if (c.dep.kind === 'db') {
          push([...h, svcFn, st, 'database/sql.(*DB).QueryContext', 'database/sql.(*DB).queryDC', 'github.com/jackc/pgx/v5/stdlib.(*Conn).QueryContext', 'github.com/jackc/pgx/v5.(*Conn).Query', 'github.com/jackc/pgx/v5/pgconn.(*PgConn).receiveMessage', 'internal/poll.(*FD).Read', 'syscall.read'], k(c.msPerReq * 0.035), e);
          push([...h, svcFn, st, 'database/sql.(*Rows).Scan', 'database/sql.convertAssignRows', ...mallocLeaf.slice(0, 1)], k(c.msPerReq * 0.02), e);
        } else if (c.dep.kind === 'cache') {
          push([...h, svcFn, 'github.com/redis/go-redis/v9.(*Client).Get', 'github.com/redis/go-redis/v9.(*baseClient).process', 'internal/poll.(*FD).Write', 'syscall.write'], k(c.msPerReq * 0.08), e);
        } else if (c.dep.kind === 'http') {
          push([...h, svcFn, 'net/http.(*Client).Do', 'net/http.(*Transport).roundTrip', 'net/http.(*persistConn).writeLoop', 'crypto/tls.(*Conn).Write', 'crypto/aes.(*gcmAsm).Seal'], k(c.msPerReq * 0.012), e);
        } else if (c.dep.kind === 'queue') {
          push([...h, svcFn, 'github.com/segmentio/kafka-go.(*Writer).WriteMessages', 'github.com/segmentio/kafka-go.(*Writer).produce', 'github.com/segmentio/kafka-go/protocol.(*encoder).writeBytes'], k(c.msPerReq * 0.05), e);
        }
      }
    } else if (type === 'alloc_space') {
      const kb = (x: number) => k(x * 1024);
      push([...h, 'encoding/json.Marshal', 'encoding/json.(*encodeState).marshal', 'bytes.(*Buffer).grow', 'runtime.growslice'], kb(18), e);
      if (isPost) push([...h, 'encoding/json.(*Decoder).Decode', 'encoding/json.(*Decoder).refill', 'runtime.growslice'], kb(9), e);
      push([...h, svcFn, ...mallocLeaf.slice(0, 1)], kb(6), e);
      push([...h.slice(0, 2), 'net/http.(*conn).readRequest', 'net/textproto.(*Reader).ReadMIMEHeader', 'runtime.makemap_small'], kb(4), e);
      push([...h, 'log/slog.(*Logger).Info', 'log/slog.(*JSONHandler).Handle', 'log/slog.(*handleState).appendString', 'runtime.growslice'], kb(3 * (1 - regress * 0.6)), e);
      if (e === top) push([...h, svcFn, `${mod}/internal/${domain}.validate`, 'regexp.MustCompile', 'regexp.compile', 'regexp/syntax.(*compiler).init', 'runtime.newobject'], kb(22 * regress), e);
      for (const c of calls) {
        if (c.dep.kind === 'db') push([...h, svcFn, `${mod}/internal/store.(*Store).${fn}`, 'database/sql.(*Rows).Scan', 'database/sql.convertAssignRows', 'runtime.slicebytetostring'], kb(7 * c.callsPerReq), e);
        if (c.dep.kind === 'http') push([...h, svcFn, 'net/http.(*Client).Do', 'net/http.(*Transport).roundTrip', 'bufio.NewReaderSize'], kb(4), e);
      }
    } else if (type === 'lock') {
      for (const c of calls) {
        if (c.dep.kind === 'db') push([...h, svcFn, `${mod}/internal/store.(*Store).${fn}`, 'database/sql.(*DB).conn', 'sync.(*Mutex).Lock', 'sync.(*Mutex).lockSlow'], k(c.msPerReq * 4e3), e);
      }
      push([...h.slice(0, 6), 'go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).OnEnd', 'sync.(*Mutex).Lock'], k(800), e);
    }
  }
  const total = out.reduce((a, x) => a + x.value, 0) || 1;
  if (type === 'cpu') {
    push(['runtime.gcBgMarkWorker', 'runtime.gcDrain', 'runtime.scanobject', 'runtime.greyobject'], total * 0.11);
    push(['runtime.gcBgMarkWorker', 'runtime.gcDrain', 'runtime.markroot', 'runtime.scanstack'], total * 0.03);
    push(['runtime.bgsweep', 'runtime.sweepone', 'runtime.(*sweepLocked).sweep'], total * 0.02);
    push(['runtime.mcall', 'runtime.park_m', 'runtime.schedule', 'runtime.findRunnable', 'runtime.netpoll', 'runtime.kevent'], total * 0.045);
    push(['runtime.goexit', 'go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).processQueue', 'go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc.(*client).UploadTraces', 'google.golang.org/protobuf/proto.MarshalOptions.marshal'], total * 0.018);
    push(['runtime.goexit', 'github.com/tasktrooper/sdk-go/profiling.(*profiler).loop', 'runtime/pprof.(*profileBuilder).build', 'compress/flate.(*compressor).deflate'], total * 0.006);
  } else if (type === 'alloc_space') {
    push(['runtime.goexit', 'go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).processQueue', 'go.opentelemetry.io/otel/exporters/otlp/otlptrace/internal/tracetransform.Spans', 'runtime.makeslice'], total * 0.05);
  } else if (type === 'inuse_space') {
    const mb = (x: number) => x * 1024 * 1024;
    push(['runtime.goexit', 'net/http.(*conn).serve', 'bufio.NewReaderSize'], mb(4) + s.rpm * 90);
    push(['runtime.goexit', 'net/http.(*conn).serve', 'bufio.NewWriterSize'], mb(2) + s.rpm * 60);
    push(['main.main', `${mod}/internal/store.Open`, 'github.com/jackc/pgx/v5/pgxpool.NewWithConfig', 'github.com/jackc/pgx/v5/pgconn.connect', 'bufio.NewReaderSize'], mb(6));
    push(['main.main', `${mod}/internal/catalog.NewCache`, `${mod}/internal/cache.(*LRU).Add`, 'runtime.mapassign_faststr', 'runtime.newobject'], mb(s.id === 'catalog-api' ? 180 : 24));
    push(['runtime.goexit', 'go.opentelemetry.io/otel/sdk/trace.NewBatchSpanProcessor', 'runtime.makechan'], mb(8));
    push(['runtime.goexit', 'github.com/segmentio/kafka-go.(*Writer).batchMessages', 'runtime.growslice'], mb(3));
    push(['main.main', 'regexp.MustCompile', 'regexp.compile'], mb(0.4));
  } else if (type === 'goroutines') {
    const conc = Math.max(8, (s.rpm / 60) * (s.p95 / 1000) * 1.8);
    push(['runtime.goexit', 'net/http.(*conn).serve', 'net/http.(*conn).readRequest', 'bufio.(*Reader).Peek', 'internal/poll.(*pollDesc).waitRead', 'runtime.gopark'], conc * 3);
    push(['runtime.goexit', 'net/http.(*conn).serve', 'net/http.serverHandler.ServeHTTP', 'github.com/go-chi/chi/v5.(*Mux).ServeHTTP', 'database/sql.(*DB).conn', 'runtime.gopark'], conc * 0.5);
    push(['runtime.goexit', 'net/http.(*persistConn).readLoop', 'internal/poll.(*pollDesc).waitRead', 'runtime.gopark'], 40);
    push(['runtime.goexit', 'net/http.(*persistConn).writeLoop', 'runtime.selectgo', 'runtime.gopark'], 40);
    push(['runtime.goexit', 'github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck', 'runtime.selectgo', 'runtime.gopark'], 1);
    push(['runtime.goexit', 'go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).processQueue', 'runtime.selectgo', 'runtime.gopark'], 1);
    push(['runtime.goexit', 'net/http.(*Server).Serve', 'net.(*TCPListener).Accept', 'internal/poll.(*pollDesc).waitRead', 'runtime.gopark'], 1);
    push(['runtime.goexit', 'os/signal.signal_recv', 'runtime.gopark'], 1);
  } else if (type === 'lock') {
    push(['runtime.goexit', `${mod}/internal/cache.(*LRU).Get`, 'sync.(*RWMutex).RLock', 'sync.runtime_SemacquireRWMutexR'], total * 0.12);
  }
  return out;
}

function nodeStacks(s: Service, type: ProfileType, regress: number): Stack[] {
  const out: Stack[] = [];
  const push = (frames: string[], value: number, e?: Endpoint) => value > 0 && out.push({ frames, value, route: e?.route, method: e?.method });
  const eps = endpointsFor(s.id);
  const top = [...eps].sort((a, b) => b.rpm - a.rpm)[0];
  for (const e of eps) {
    const b = endpointBreakdown(e);
    const fn = handlerName(e).replace(/^./, (c) => c.toLowerCase());
    const domain = e.sub ? 'consumers' : (e.route.split('/').filter((p) => p && !/^v\d+$/.test(p) && !p.startsWith(':'))[0] ?? 'core').replace(/[^a-z]/gi, '');
    const base = e.sub
      ? ['(root)', 'processTicksAndRejections (node:internal/process/task_queues)', 'runConsumer (node_modules/kafkajs/src/consumer/runner.js)', `${fn} (src/consumers/${e.sub.process}.js)`]
      : ['(root)', 'processTicksAndRejections (node:internal/process/task_queues)', 'handle (node_modules/express/lib/router/layer.js)', 'next (node_modules/express/lib/router/route.js)', `${fn} (src/routes/${domain}.js)`];
    const svcFn = `${fn} (src/services/${domain}.service.js)`;
    const k = (x: number) => x * (e.rpm / 60);
    const app = b.parts.app;
    if (type === 'cpu') {
      push([...base, svcFn], k(app * 0.45), e);
      push([...base, 'json (node_modules/express/lib/response.js)', 'stringify (native)'], k(app * 0.16), e);
      push([...base, 'info (node_modules/pino/lib/proto.js)', 'asJson (node_modules/pino/lib/tools.js)'], k(app * 0.07 * (1 - regress * 0.6)), e);
      push([...base, 'end (node_modules/@tasktrooper/node/dist/span.js)', 'onEnd (node_modules/@opentelemetry/sdk-trace-base/build/src/export/BatchSpanProcessorBase.js)'], k(app * 0.03), e);
      if (e === top) push([...base, svcFn, 'cloneDeep (node_modules/lodash/lodash.js)', 'baseClone (node_modules/lodash/lodash.js)', 'arrayEach (node_modules/lodash/lodash.js)'], k(app * 0.22 * regress), e);
      for (const c of endpointCalls(e)) {
        if (c.dep.kind === 'db') push([...base, svcFn, 'query (node_modules/pg/lib/client.js)', 'parse (node_modules/pg-protocol/dist/parser.js)', 'parseDataRowMessage (node_modules/pg-protocol/dist/parser.js)'], k(c.msPerReq * 0.05), e);
        if (c.dep.kind === 'cache') push([...base, svcFn, 'sendCommand (node_modules/ioredis/built/Redis.js)', 'write (node:net)'], k(c.msPerReq * 0.08), e);
        if (c.dep.kind === 'http') push([...base, svcFn, 'request (node_modules/undici/lib/api/api-request.js)', 'writeGeneric (node:internal/stream_base_commons)'], k(c.msPerReq * 0.02), e);
        if (c.dep.kind === 'queue') push([...base, svcFn, 'send (node_modules/kafkajs/src/producer/index.js)', 'encode (node_modules/kafkajs/src/protocol/recordBatch/v0/index.js)'], k(c.msPerReq * 0.06), e);
      }
    } else if (type === 'inuse_space') {
      push([...base, svcFn, '(anonymous) (src/services/' + domain + '.service.js)'], 2 * 1024 * 1024, e);
    }
  }
  const total = out.reduce((a, x) => a + x.value, 0) || 1;
  if (type === 'cpu') {
    push(['(root)', '(garbage collector)'], total * 0.12);
    push(['(root)', '(program)'], total * 0.05);
    push(['(root)', 'processTimers (node:internal/timers)', 'listOnTimeout (node:internal/timers)', 'flush (node_modules/@tasktrooper/node/dist/exporter.js)', 'encode (node_modules/protobufjs/src/writer.js)'], total * 0.02);
  } else {
    push(['(root)', 'Module._load (node:internal/modules/cjs/loader)', 'compileFunction (node:vm)'], 38 * 1024 * 1024);
    push(['(root)', 'createPool (node_modules/pg-pool/index.js)', 'Client (node_modules/pg/lib/client.js)'], 6 * 1024 * 1024);
    push(['(root)', 'BatchSpanProcessorBase (node_modules/@opentelemetry/sdk-trace-base/build/src/export/BatchSpanProcessorBase.js)'], 9 * 1024 * 1024);
  }
  return out;
}

/** Root-first tree from stacks; children sorted by value; nodes < 0.05 % of total folded into "[pruned]". */
function buildTree(stacks: Stack[]): FlameNode {
  const root: FlameNode = { name: 'all', value: 0, self: 0, children: [] };
  const idx = new Map<FlameNode, Map<string, FlameNode>>();
  for (const st of stacks) {
    root.value += st.value;
    let n = root;
    for (const f of st.frames) {
      let m = idx.get(n);
      if (!m) { m = new Map(); idx.set(n, m); }
      let c = m.get(f);
      if (!c) { c = { name: f, value: 0, self: 0, children: [] }; m.set(f, c); n.children.push(c); }
      c.value += st.value;
      n = c;
    }
    n.self += st.value;
  }
  const min = root.value * 0.0005;
  const finish = (n: FlameNode) => {
    const keep = n.children.filter((c) => c.value >= min);
    const pruned = n.children.filter((c) => c.value < min).reduce((a, c) => a + c.value, 0);
    n.children = keep.sort((a, b) => b.value - a.value);
    if (pruned > 0) n.children.push({ name: '[pruned]', value: pruned, self: pruned, children: [] });
    n.children.forEach(finish);
  };
  finish(root);
  return root;
}

function rangeMs(range: TimeRange) { const r = RANGE_POINTS[range]; return r.points * r.stepMs; }
const latestDeploy = (s: Service) => deploysOf(s.id)[0]?.ts ?? NOW - 2 * 3600_000;
/** Share of [from, to] that ran the newest version (regression present). */
function newShare(s: Service, from: number, to: number) {
  const d = latestDeploy(s);
  return to <= d ? 0 : from >= d ? 1 : (to - d) / (to - from);
}

interface MockOpts { from: number; to: number; filter: ProfileFilter }
function mockStacks(s: Service, type: ProfileType, o: MockOpts): { stacks: Stack[]; profiles: number; unit: ProfileUnit } {
  const rt = runtimeOf(s);
  const info = mockTypes(s.id).find((x) => x.type === type);
  if (!rt || !info) return { stacks: [], profiles: 0, unit: 'nanoseconds' };
  const versions = mockVersions(s);
  const f = o.filter;
  const regress = f.version ? (f.version === versions[0] ? 1 : 0) : newShare(s, o.from, o.to);
  let stacks = rt === 'go' ? goStacks(s, type, regress) : nodeStacks(s, type, regress);
  if (f.route) stacks = stacks.filter((x) => x.route === f.route && (!f.method || x.method === f.method));
  const hosts = f.host ? 1 : info.hosts.length;
  const minutes = Math.max(1, (o.to - o.from) / 60_000);
  const profiles = Math.round(hosts * minutes);
  // scale the relative weights to a believable absolute total for the window
  const all = (rt === 'go' ? goStacks(s, type, regress) : nodeStacks(s, type, regress)).reduce((a, x) => a + x.value, 0) || 1;
  const perHostPerProfile: Record<string, number> = {
    cpu: (rt === 'go' ? 0.55 : 0.4) * (1 + regress * (s.id === 'payments-api' ? 0.3 : 0.12)) * 10e9 * Math.max(0.6, s.rpm / Math.max(1, s.instances) / 1500),
    alloc_space: 60 * (s.rpm / Math.max(1, s.instances) / 60) * 48 * 1024,
    inuse_space: 1, goroutines: 1, lock: 60 * 0.35e9,
  };
  // gauge stacks are already absolute per host snapshot; the api sums them over profiles
  const target = isGaugeType(type) ? all * profiles : perHostPerProfile[type] * profiles;
  const hostMul = f.host ? jitter(f.host, 0.3) : 1;
  const scale = (target / all) * hostMul;
  return { stacks: stacks.map((x) => ({ ...x, value: Math.round(x.value * scale) })), profiles, unit: info.unit };
}

function mockFlame(serviceId: string, type: ProfileType, range: TimeRange, f: ProfileFilter): FlameGraphData {
  const s = services.find((x) => x.id === serviceId);
  const to = NOW;
  const from = NOW - rangeMs(range);
  if (!s) return { type, unit: 'nanoseconds', total: 0, profiles: 0, from, to, root: { name: 'all', value: 0, self: 0, children: [] } };
  const { stacks, profiles, unit } = mockStacks(s, type, { from, to, filter: f });
  const root = buildTree(stacks);
  return { type, unit, total: root.value, profiles: stacks.length ? profiles : 0, from, to, root };
}

function mergeDiff(a: FlameNode, b: FlameNode): DiffNode {
  const byName = new Map<string, { cur?: FlameNode; base?: FlameNode }>();
  for (const c of a.children) byName.set(c.name, { cur: c });
  for (const c of b.children) byName.set(c.name, { ...byName.get(c.name), base: c });
  const empty: FlameNode = { name: '', value: 0, self: 0, children: [] };
  const children = [...byName.entries()].map(([name, { cur, base }]) => mergeDiff({ ...(cur ?? empty), name }, { ...(base ?? empty), name }))
    .sort((x, y) => y.value + y.baseValue - (x.value + x.baseValue));
  return { name: a.name || b.name, value: a.value, self: a.self, baseValue: b.value, baseSelf: b.self, children };
}

function mockDiff(serviceId: string, type: ProfileType, cmp: ProfileComparison, f: ProfileFilter): ProfileDiffData {
  const s = services.find((x) => x.id === serviceId);
  const unit = mockTypes(serviceId).find((x) => x.type === type)?.unit ?? 'nanoseconds';
  const window = 3600_000;
  const [cur, base] = cmp.kind === 'version'
    ? [{ from: NOW - window, to: NOW, filter: { ...f, version: cmp.version } }, { from: NOW - window, to: NOW, filter: { ...f, version: cmp.baseVersion } }]
    : [{ from: cmp.from, to: cmp.to, filter: { ...f, version: undefined } }, { from: cmp.baseFrom, to: cmp.baseTo, filter: { ...f, version: undefined } }];
  if (!s) return { type, unit, total: 0, baseTotal: 0, root: { name: 'all', value: 0, self: 0, baseValue: 0, baseSelf: 0, children: [] } };
  const a = buildTree(mockStacks(s, type, cur).stacks);
  const b = buildTree(mockStacks(s, type, base).stacks);
  return { type, unit, total: a.value, baseTotal: b.value, root: mergeDiff(a, b) };
}

function mockProfileSeries(serviceId: string, type: ProfileType, range: TimeRange, f: ProfileFilter): ProfileSeries {
  const s = services.find((x) => x.id === serviceId);
  const ts = timestamps(range);
  const info = mockTypes(serviceId).find((x) => x.type === type);
  if (!s || !info) return { timestamps: ts, series: [] };
  const hosts = f.host ? [f.host] : info.hosts;
  const versions = mockVersions(s);
  const deploy = latestDeploy(s);
  const one = mockStacks(s, type, { from: NOW - 60_000, to: NOW, filter: { version: versions[0] } });
  const oldOne = mockStacks(s, type, { from: NOW - 60_000, to: NOW, filter: { version: versions[1] ?? versions[0] } });
  const sum = (st: Stack[]) => st.reduce((a, x) => a + x.value, 0) / Math.max(1, info.hosts.length);
  // per host, per minute: cpu → cores (10 s of samples per profile), rates per minute, gauges as is
  const toUnit = (v: number) => (type === 'cpu' || type === 'wall' ? v / 10e9 : v);
  const now = toUnit(sum(one.stacks));
  const before = toUnit(sum(oldOne.stacks));
  return {
    timestamps: ts,
    series: hosts.slice(0, 8).map((host) => {
      const base = mockSeries(`prof-${type}-${host}`, range, { base: 1, noise: 0.08, daily: 0.25 });
      return {
        id: host, label: host.replace(`${s.name}-`, ''),
        data: base.map((m, i) => {
          if (f.version && (ts[i] >= deploy) !== (f.version === versions[0])) return 0;
          return Number(((ts[i] >= deploy ? now : before) * m * jitter(host, 0.25)).toFixed(4));
        }),
      };
    }),
  };
}
