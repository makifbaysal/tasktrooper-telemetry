import { context } from '@opentelemetry/api';
import { suppressTracing } from '@opentelemetry/core';
import { hostname } from 'node:os';
import { sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import { gzip as gzipCb } from 'node:zlib';
import type { ProfileType, ResolvedProfiling } from './config';
import { recordError } from './errors';

/**
 * Continuous profiling (ADR-022, .ai/contracts.md §24): `node:inspector` CPU profiler + V8 sampling heap profiler,
 * converted to folded stacks (`root;…;leaf <value>`) and uploaded to `POST /v1/profiles?format=folded`.
 *
 * Frame names: `fn (relative/path.js)` — cwd and `file://` stripped, no line/column, `(anonymous)` for empty names.
 * V8 pseudo-frames: `(root)` is dropped, `(idle)` is excluded from `cpu` and becomes `[idle]` in `wall`,
 * `(program)` (native / unattributed VM time) → `[program]`, `(garbage collector)` → `[gc]`.
 */

const gzip = promisify(gzipCb);

/** Maximum frames per stack (.ai/contracts.md §24); deeper stacks keep the leaf side under a `[truncated]` root frame. */
export const MAX_STACK_DEPTH = 128;

export interface CallFrame {
  functionName: string;
  url: string;
  scriptId?: string;
  lineNumber?: number;
  columnNumber?: number;
}

/** Subset of the DevTools `Profiler.Profile` (.cpuprofile) the converter reads. */
export interface CpuProfile {
  nodes: { id: number; callFrame: CallFrame; children?: number[] }[];
  /** Microseconds. */
  startTime: number;
  /** Microseconds. */
  endTime: number;
  samples?: number[];
  /** Microseconds between consecutive samples (first one relative to startTime). */
  timeDeltas?: number[];
}

export interface HeapProfileNode {
  callFrame: CallFrame;
  /** Estimated bytes allocated at this node (excluding children). */
  selfSize: number;
  id: number;
  children?: HeapProfileNode[];
}

/** Subset of the DevTools `HeapProfiler.SamplingHeapProfile`. */
export interface HeapProfile {
  head: HeapProfileNode;
}

const IDLE = '[idle]';
const SKIP = '';

function sanitize(s: string): string {
  return s.replace(/[;\r\n]/g, ' ');
}

/** Script URL → path relative to `cwd` (`file://` stripped, `/` separators). Non-file URLs (`node:…`) are kept. */
export function relativePath(url: string, cwd: string = process.cwd()): string {
  if (!url) return '';
  let p = url;
  if (p.startsWith('file://')) {
    try {
      p = fileURLToPath(p);
    } catch {
      p = p.slice(7);
    }
  }
  const base = cwd.endsWith(sep) ? cwd : cwd + sep;
  if (p.startsWith(base)) p = p.slice(base.length);
  return p.split(sep).join('/');
}

/** Folded frame name (`fn (path/to/file.js)`); `SKIP` for frames that are dropped, `IDLE` for the idle pseudo-frame. */
export function frameName(cf: CallFrame, cwd: string = process.cwd()): string {
  const fn = cf.functionName ?? '';
  if (!cf.url) {
    switch (fn) {
      case '(root)':
        return SKIP;
      case '(idle)':
        return IDLE;
      case '(program)':
        return '[program]';
      case '(garbage collector)':
        return '[gc]';
    }
  }
  const name = sanitize(fn || '(anonymous)');
  const file = relativePath(cf.url, cwd);
  return file ? `${name} (${sanitize(file)})` : name;
}

function capDepth(frames: string[]): string[] {
  return frames.length > MAX_STACK_DEPTH ? ['[truncated]', ...frames.slice(frames.length - (MAX_STACK_DEPTH - 1))] : frames;
}

export interface CpuStacks {
  /** On-CPU stacks (idle excluded) → nanoseconds. */
  stacks: Map<string, number>;
  /** Idle time, nanoseconds. */
  idle: number;
}

/**
 * Converts a V8 CPU profile into folded stacks. Each sample lasts until the next one (the last until `endTime`), so the
 * values add up to the profiled wall time; samples are µs → values ns.
 */
export function cpuProfileToStacks(profile: CpuProfile, cwd: string = process.cwd()): CpuStacks {
  const byId = new Map<number, CpuProfile['nodes'][number]>();
  const parent = new Map<number, number>();
  for (const n of profile.nodes ?? []) {
    byId.set(n.id, n);
    for (const c of n.children ?? []) parent.set(c, n.id);
  }
  const memo = new Map<number, string | null>(); // folded stack of a node; null = idle; '' = nothing
  const stackOf = (id: number): string | null => {
    const hit = memo.get(id);
    if (hit !== undefined) return hit;
    const frames: string[] = [];
    let idle = false;
    for (let cur: number | undefined = id; cur !== undefined; cur = parent.get(cur)) {
      const node = byId.get(cur);
      if (!node) break;
      const f = frameName(node.callFrame, cwd);
      if (f === IDLE) idle = true;
      else if (f !== SKIP) frames.push(f);
    }
    const out = idle && frames.length === 0 ? null : capDepth(frames.reverse()).join(';');
    memo.set(id, out);
    return out;
  };

  const stacks = new Map<string, number>();
  let idle = 0;
  const samples = profile.samples ?? [];
  const deltas = profile.timeDeltas ?? [];
  let ts = profile.startTime;
  const times: number[] = new Array(samples.length);
  for (let i = 0; i < samples.length; i++) {
    ts += deltas[i] ?? 0;
    times[i] = ts;
  }
  for (let i = 0; i < samples.length; i++) {
    const next = i + 1 < samples.length ? times[i + 1]! : profile.endTime;
    const ns = Math.round(Math.max(0, next - times[i]!) * 1000);
    if (ns <= 0) continue;
    const s = stackOf(samples[i]!);
    if (s === null) idle += ns;
    else if (s) stacks.set(s, (stacks.get(s) ?? 0) + ns);
  }
  return { stacks, idle };
}

/** Converts a V8 sampling heap profile into folded stacks (value = bytes). */
export function heapProfileToStacks(profile: HeapProfile, cwd: string = process.cwd()): Map<string, number> {
  const stacks = new Map<string, number>();
  if (!profile?.head) return stacks;
  const todo: [HeapProfileNode, string[]][] = [[profile.head, []]];
  while (todo.length) {
    const [node, parentFrames] = todo.pop()!;
    const f = frameName(node.callFrame, cwd);
    const frames = f === SKIP || f === IDLE ? parentFrames : [...parentFrames, f];
    const size = Math.round(node.selfSize ?? 0);
    if (size > 0 && frames.length) {
      const key = capDepth(frames).join(';');
      stacks.set(key, (stacks.get(key) ?? 0) + size);
    }
    for (const c of node.children ?? []) todo.push([c, frames]);
  }
  return stacks;
}

/** `frame;frame;frame value` lines (zero values dropped). */
export function foldedText(stacks: Map<string, number>): string {
  let out = '';
  for (const [stack, v] of stacks) if (v > 0 && stack) out += `${stack} ${Math.round(v)}\n`;
  return out;
}

export interface ProfileMeta {
  service: string;
  env?: string;
  version?: string;
  host?: string;
  type: ProfileType;
  unit: 'nanoseconds' | 'bytes';
  /** Unix ms. */
  start: number;
  /** Unix ms. */
  end: number;
}

export function unitOf(type: ProfileType): ProfileMeta['unit'] {
  return type === 'cpu' || type === 'wall' ? 'nanoseconds' : 'bytes';
}

/** Query string of `POST /v1/profiles` (.ai/contracts.md §24). */
export function profileQuery(meta: ProfileMeta): string {
  const q = new URLSearchParams({ format: 'folded', type: meta.type, unit: meta.unit, service: meta.service });
  if (meta.env) q.set('env', meta.env);
  if (meta.version) q.set('version', meta.version);
  if (meta.host) q.set('host', meta.host);
  q.set('runtime', 'node');
  q.set('start', String(Math.floor(meta.start)));
  q.set('end', String(Math.ceil(meta.end)));
  return q.toString();
}

type FetchLike = (url: string, init: { method: string; headers: Record<string, string>; body: Uint8Array; signal?: AbortSignal }) => Promise<{ status: number; headers?: { get(name: string): string | null } }>;

const MIN_BACKOFF_MS = 1000;
const MAX_BACKOFF_MS = 60_000;

/**
 * Uploads folded profiles. No queue (.ai/contracts.md §24): a failed profile is dropped. 429 / 5xx / network errors
 * start an exponential backoff (1 s → 60 s, jitter, `Retry-After` honoured) during which profiles are dropped; other
 * 4xx drop the profile. Never throws; failures count as `trooper.sdk.errors{component=exporter.profiles}`.
 */
export class ProfileUploader {
  private backoffMs = 0;
  private until = 0;

  constructor(
    private readonly url: string,
    private readonly headers: Record<string, string>,
    private readonly timeoutMs = 10_000,
    private readonly fetchImpl: FetchLike = globalThis.fetch as unknown as FetchLike,
    private readonly now: () => number = Date.now,
  ) {}

  /** True while uploads are suppressed by backoff. */
  get backingOff(): boolean {
    return this.now() < this.until;
  }

  async upload(meta: ProfileMeta, folded: string): Promise<boolean> {
    if (!folded) return false;
    if (this.backingOff) return false;
    try {
      const body = new Uint8Array(await gzip(Buffer.from(folded, 'utf8')));
      const res = await context.with(suppressTracing(context.active()), () =>
        this.fetchImpl(`${this.url}?${profileQuery(meta)}`, {
          method: 'POST',
          headers: { ...this.headers, 'content-type': 'text/plain; charset=utf-8', 'content-encoding': 'gzip' },
          body,
          signal: AbortSignal.timeout(this.timeoutMs),
        }),
      );
      if (res.status >= 200 && res.status < 300) {
        this.backoffMs = 0;
        return true;
      }
      if (res.status === 429 || res.status >= 500) this.backoff(res.headers?.get('retry-after'));
      recordError('exporter.profiles', new Error(`HTTP ${res.status}`));
      return false;
    } catch (err) {
      this.backoff(null);
      recordError('exporter.profiles', err);
      return false;
    }
  }

  private backoff(retryAfter: string | null | undefined): void {
    this.backoffMs = Math.min(MAX_BACKOFF_MS, this.backoffMs ? this.backoffMs * 2 : MIN_BACKOFF_MS);
    let wait = this.backoffMs * (0.5 + Math.random() * 0.5);
    const ra = retryAfter ? Number(retryAfter) : NaN;
    if (Number.isFinite(ra) && ra > 0) wait = Math.max(wait, Math.min(ra * 1000, MAX_BACKOFF_MS));
    this.until = this.now() + wait;
  }
}

interface InspectorSession {
  connect(): void;
  disconnect(): void;
  post(method: string, params?: object): Promise<unknown>;
}

export interface ProfilerOptions {
  config: ResolvedProfiling;
  uploader: Pick<ProfileUploader, 'upload'>;
  service: string;
  env?: string;
  version?: string;
  host?: string;
  cwd?: string;
  /** Test hook. */
  sessionFactory?: () => Promise<InspectorSession>;
}

async function defaultSession(): Promise<InspectorSession> {
  const { Session } = await import('node:inspector/promises');
  return new Session() as unknown as InspectorSession;
}

/**
 * Runs the profiling loop: every period a CPU profile of `cpuDurationMs` (types `cpu` / `wall`) and a heap snapshot of the
 * sampling heap profiler (`inuse_space`, or `alloc_space` since the previous period). Timers are `unref()`ed. Never throws.
 */
export class ContinuousProfiler {
  private session?: InspectorSession;
  private timer?: NodeJS.Timeout;
  private stopped = false;
  private cpuRun?: Promise<void>;
  private wake?: () => void;
  private heapSince = 0;
  private heapRunning = false;
  private readonly cpuTypes: ProfileType[];
  private readonly heapType?: 'inuse_space' | 'alloc_space';
  private readonly cwd: string;

  constructor(private readonly opts: ProfilerOptions) {
    const t = opts.config.types;
    this.cpuTypes = t.filter((x) => x === 'cpu' || x === 'wall');
    this.heapType = t.includes('inuse_space') ? 'inuse_space' : t.includes('alloc_space') ? 'alloc_space' : undefined;
    this.cwd = opts.cwd ?? process.cwd();
  }

  /** Starts profiling in the background. */
  start(): Promise<void> {
    return this.begin().catch((err) => {
      recordError('profiler', err);
    });
  }

  private async begin(): Promise<void> {
    if ((process.features as { inspector?: boolean } | undefined)?.inspector === false) return;
    const s = await (this.opts.sessionFactory ?? defaultSession)();
    if (this.stopped) return;
    s.connect();
    this.session = s;
    if (this.cpuTypes.length) {
      await s.post('Profiler.enable');
      await s.post('Profiler.setSamplingInterval', { interval: this.opts.config.cpuSamplingIntervalUs });
    }
    if (this.heapType) {
      await s.post('HeapProfiler.enable');
      await this.startHeap();
    }
    if (this.stopped) return;
    this.timer = setInterval(() => this.tick(), this.opts.config.periodMs);
    this.timer.unref?.();
    this.cpuRun = this.runCpu();
  }

  private async startHeap(): Promise<void> {
    const alloc = this.heapType === 'alloc_space';
    await this.session!.post('HeapProfiler.startSampling', {
      samplingInterval: this.opts.config.heapSamplingIntervalBytes,
      includeObjectsCollectedByMajorGC: alloc,
      includeObjectsCollectedByMinorGC: alloc,
    });
    this.heapRunning = true;
    this.heapSince = Date.now();
  }

  private tick(): void {
    if (this.stopped) return;
    this.collectHeap().catch((err) => recordError('profiler', err));
    if (!this.cpuRun) this.cpuRun = this.runCpu();
  }

  private meta(type: ProfileType, start: number, end: number): ProfileMeta {
    const o = this.opts;
    return { service: o.service, env: o.env, version: o.version, host: o.host ?? safeHostname(), type, unit: unitOf(type), start, end };
  }

  private async collectHeap(): Promise<void> {
    const s = this.session;
    if (!s || !this.heapType || !this.heapRunning) return;
    const start = this.heapSince;
    const end = Date.now();
    let profile: HeapProfile | undefined;
    if (this.heapType === 'inuse_space') {
      profile = ((await s.post('HeapProfiler.getSamplingProfile')) as { profile: HeapProfile }).profile;
    } else {
      this.heapRunning = false;
      profile = ((await s.post('HeapProfiler.stopSampling')) as { profile: HeapProfile }).profile;
      if (!this.stopped) await this.startHeap();
    }
    this.heapSince = end;
    const folded = foldedText(heapProfileToStacks(profile, this.cwd));
    await this.opts.uploader.upload(this.meta(this.heapType, start, end), folded);
  }

  private async runCpu(): Promise<void> {
    const s = this.session;
    try {
      if (!s || !this.cpuTypes.length || this.stopped) return;
      const start = Date.now();
      await s.post('Profiler.start');
      await new Promise<void>((resolve) => {
        if (this.stopped) return resolve();
        const t = setTimeout(resolve, this.opts.config.cpuDurationMs);
        t.unref?.();
        this.wake = () => {
          clearTimeout(t);
          resolve();
        };
      });
      this.wake = undefined;
      const { profile } = (await s.post('Profiler.stop')) as { profile: CpuProfile };
      const end = Date.now();
      const { stacks, idle } = cpuProfileToStacks(profile, this.cwd);
      for (const type of this.cpuTypes) {
        let folded = foldedText(stacks);
        if (type === 'wall' && idle > 0) folded += `${IDLE} ${idle}\n`;
        await this.opts.uploader.upload(this.meta(type, start, end), folded);
      }
    } catch (err) {
      recordError('profiler', err);
    } finally {
      this.cpuRun = undefined;
    }
  }

  /** Stops profiling: the running CPU profile is cut short and uploaded (bounded by `timeoutMs`). Never rejects. */
  async stop(timeoutMs = 5000): Promise<void> {
    if (this.stopped) return;
    this.stopped = true;
    if (this.timer) clearInterval(this.timer);
    try {
      this.wake?.();
      const pending = this.cpuRun;
      if (pending) {
        await Promise.race([
          pending,
          new Promise<void>((r) => {
            const t = setTimeout(r, timeoutMs);
            t.unref?.();
          }),
        ]);
      }
      const s = this.session;
      if (s) {
        if (this.heapRunning) await s.post('HeapProfiler.stopSampling').catch(() => {});
        this.heapRunning = false;
        await s.post('Profiler.disable').catch(() => {});
        await s.post('HeapProfiler.disable').catch(() => {});
        s.disconnect();
      }
    } catch (err) {
      recordError('profiler', err);
    }
    this.session = undefined;
  }
}

function safeHostname(): string | undefined {
  try {
    return hostname();
  } catch {
    return undefined;
  }
}
