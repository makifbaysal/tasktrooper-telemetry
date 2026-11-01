/**
 * Continuous profiling (ADR-022, .ai/contracts.md §24).
 *
 * `GET /profiles/types | flamegraph | diff | top | series`.
 */
import { services } from './apm';
import { qs, remote, remoteStatus } from './remote';
import type { TimeRange } from './types';

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

// ---------------------------------------------------------------- tree helpers 
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
  return state(`/profiles/types${qs({ service: svcName(serviceId), range })}`, undefined,
    (r: { types?: ProfileTypeInfo[] | null } | null) => (r?.types ?? []).map((t) => ({ ...t, hosts: t.hosts ?? [], versions: t.versions ?? [], routes: num(t.routes) })));
}

export function flameGraph(serviceId: string, type: ProfileType, range: TimeRange, f: ProfileFilter = {}): Loadable<FlameGraphData> {
  return state(`/profiles/flamegraph${qs({ service: svcName(serviceId), type, range, ...filterQs(f) })}`, undefined,
    (r: FlameGraphData) => ({ ...r, total: num(r.total), profiles: num(r.profiles), root: mapFlame(r.root) }));
}

/** `range` scopes version-vs-version mode (api default 24h); window mode uses its own from/to. */
export function profileDiff(serviceId: string, type: ProfileType, cmp: ProfileComparison, f: ProfileFilter = {}, range: TimeRange = '24h'): Loadable<ProfileDiffData> {
  const sel = cmp.kind === 'version' ? { baseVersion: cmp.baseVersion, version: cmp.version, range } : { baseFrom: cmp.baseFrom, baseTo: cmp.baseTo, from: cmp.from, to: cmp.to };
  const { version: _v, ...rest } = filterQs(f);
  void _v;
  return state(`/profiles/diff${qs({ service: svcName(serviceId), type, ...rest, ...sel })}`, undefined,
    (r: ProfileDiffData) => ({ ...r, total: num(r.total), baseTotal: num(r.baseTotal), root: mapDiff(r.root) }));
}

export function topFunctions(serviceId: string, type: ProfileType, range: TimeRange, f: ProfileFilter = {}): Loadable<TopFunctions> {
  return state(`/profiles/top${qs({ service: svcName(serviceId), type, range, ...filterQs(f), limit: 50 })}`, undefined,
    (r: TopFunctions) => ({ unit: r.unit, total: num(r.total), functions: r.functions ?? [] }));
}

/** cpu/wall: avg cores per host; others: per-minute average per host (≤ 8 hosts, rest "Other"). */
export function profileSeries(serviceId: string, type: ProfileType, range: TimeRange, f: ProfileFilter = {}): Loadable<ProfileSeries> {
  return state(`/profiles/series${qs({ service: svcName(serviceId), type, range, env: f.env, version: f.version })}`, undefined,
    (r: ProfileSeries) => ({ timestamps: r.timestamps ?? [], series: (r.series ?? []).map((s) => ({ ...s, data: (s.data ?? []).map(num) })) }));
}
