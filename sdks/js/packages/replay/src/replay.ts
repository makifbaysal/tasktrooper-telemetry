import { record } from '@rrweb/record';
import type { IntegrationClient, RumEvent } from '@trooper/browser';
import { rrwebPrivacyOptions, type PrivacyOptions } from './privacy';
import { KEY_PREFIX, ReplayStore } from './store';
import { KEEPALIVE_LIMIT, ReplayTransport } from './transport';

export const SDK_NAME = '@trooper/replay';
export const SDK_VERSION = '0.1.0';

/** A segment is sent after 5 s or once ~256 KB of (uncompressed) events are collected — contracts.md §23. */
export const SEGMENT_MS = 5000;
export const SEGMENT_BYTES = 256 * 1024;
/** Session mode: a FullSnapshot every 5 min (rrweb checkout). */
export const CHECKOUT_MS = 5 * 60 * 1000;
/** Buffer mode (onErrorSampleRate): keep the last 60 s; a checkout every 60 s guarantees a FullSnapshot in memory. */
export const BUFFER_MS = 60 * 1000;

export type ReplayMode = 'off' | 'buffer' | 'session';

export interface ReplayOptions {
  /** Set false to disable replay entirely. Default true. */
  enabled?: boolean;
  /** Fraction of sessions recorded from the start (0..1). Default 0.1. */
  sampleRate?: number;
  /**
   * Fraction of the remaining sessions that keep the last 60 s in memory and start recording when an ERROR occurs.
   * Default 1.0.
   */
  onErrorSampleRate?: number;
  /** Replace all text with `*` (length kept). Default true. */
  maskAllText?: boolean;
  /** Mask all input values. Default true. Password fields are always masked. */
  maskAllInputs?: boolean;
  /** Elements recorded as an empty box of the same size. Default `[data-trooper-block]`. */
  blockSelector?: string;
  /** Text and inputs to mask when `maskAllText` / `maskAllInputs` are off. Default `[data-trooper-mask]`. */
  maskSelector?: string;
  /** Opt subtrees out of masking (never applies to passwords). Default `[data-trooper-unmask]`. */
  unmaskSelector?: string;
}

/** rrweb event shape we rely on (`@rrweb/types` eventWithTime). */
interface RrwebEvent {
  type: number;
  data?: { source?: number; type?: number; id?: number };
  timestamp: number;
}

// rrweb EventType / IncrementalSource / MouseInteractions numbers.
const FULL_SNAPSHOT = 2;
const INCREMENTAL = 3;
const SRC_MOUSE = 2;
const SRC_INPUT = 5;
const MOUSE_CLICK = 2;

interface Ev {
  json: string;
  ts: number;
  full: boolean;
  click: boolean;
  input: boolean;
}

interface Segment {
  sessionId: string;
  events: Ev[];
  bytes: number;
  errors: number;
  rage: number;
  viewId?: string;
  viewName?: string;
}

export interface SegmentStats {
  clicks: number;
  errors: number;
  rage_clicks: number;
  inputs: number;
}

/** One `POST /v1/replay` body (contracts.md §23). `events` are rrweb events as recorded. */
export interface ReplaySegmentPayload {
  resource: { service: string; env?: string; version?: string; platform: 'web'; sdk_name: string; sdk_version: string };
  session_id: string;
  segment: number;
  start_ts: number;
  end_ts: number;
  format: 'rrweb';
  has_full_snapshot: boolean;
  view_id?: string;
  view_name?: string;
  user_id: string;
  anonymous_id: string;
  stats: SegmentStats;
  events: unknown[];
}

const clamp01 = (v: unknown, d: number): number => (typeof v === 'number' && v >= 0 && v <= 1 ? v : d);

export class Replay {
  readonly transport: ReplayTransport;
  readonly store = new ReplayStore();
  private mode: ReplayMode = 'off';
  private sessionId = '';
  private seg: Segment | undefined;
  /** Buffer mode: the segment before the last checkout (so ≥ 60 s are always kept). */
  private prev: Segment | undefined;
  private segTimer: ReturnType<typeof setTimeout> | undefined;
  private stopRecording: (() => void) | undefined;
  private lastCheckout = 0;
  private checkoutPending = false;
  private lastInputId: number | undefined;
  private teardown: (() => void)[] = [];
  private readonly privacy: PrivacyOptions;
  private readonly sampleRate: number;
  private readonly onErrorSampleRate: number;

  constructor(
    private readonly ic: IntegrationClient,
    o: ReplayOptions,
  ) {
    this.transport = new ReplayTransport(ic);
    this.sampleRate = clamp01(o.sampleRate, 0.1);
    this.onErrorSampleRate = clamp01(o.onErrorSampleRate, 1);
    this.privacy = {
      maskAllText: o.maskAllText !== false,
      maskAllInputs: o.maskAllInputs !== false,
      blockSelector: o.blockSelector || '[data-trooper-block]',
      maskSelector: o.maskSelector || '[data-trooper-mask]',
      unmaskSelector: o.unmaskSelector || '[data-trooper-unmask]',
    };
  }

  get currentMode(): ReplayMode {
    return this.mode;
  }

  start(): void {
    this.teardown.push(this.ic.onSessionStart((id) => this.safe('session', () => this.onSession(id))));
    this.teardown.push(this.ic.onEvent((e) => this.safe('event', () => this.onRumEvent(e))));
    const onHide = () => this.safe('hide', () => (document.visibilityState === 'hidden' ? this.flushKeepalive() : undefined));
    const onPageHide = () => this.safe('pagehide', () => this.flushKeepalive());
    const onStorage = (e: StorageEvent) => this.safe('storage', () => this.onStorage(e));
    addEventListener('visibilitychange', onHide);
    addEventListener('pagehide', onPageHide);
    addEventListener('storage', onStorage);
    this.teardown.push(() => {
      removeEventListener('visibilitychange', onHide);
      removeEventListener('pagehide', onPageHide);
      removeEventListener('storage', onStorage);
    });
    this.onSession(this.ic.sessionId());
  }

  // --- sampling ---------------------------------------------------------------------------------

  /**
   * Per-session decision shared by all tabs of the session (localStorage `tt_replay:<session_id>`, survives page loads).
   */
  private decide(sessionId: string): ReplayMode {
    const cur = this.store.get(sessionId);
    if (cur) return cur.m;
    let m: ReplayMode = 'off';
    if (this.ic.sampled()) {
      if (Math.random() < this.sampleRate) m = 'session';
      else if (Math.random() < this.onErrorSampleRate) m = 'buffer';
    }
    this.store.set(sessionId, { m, n: 0 });
    return m;
  }

  // --- lifecycle --------------------------------------------------------------------------------

  private onSession(id: string): void {
    if (!id || id === this.sessionId) return;
    // Finish the previous session's segment under its own id.
    if (this.mode === 'session') this.flushSegment();
    this.seg = this.prev = undefined;
    this.clearSegTimer();
    this.sessionId = id;
    this.mode = this.decide(id);
    this.store.cleanup(id); // previous sessions' entries are never needed again
    if (this.mode === 'off') {
      this.stopRrweb();
      return;
    }
    if (this.stopRecording) this.checkout();
    else this.startRrweb();
  }

  private startRrweb(): void {
    this.lastCheckout = Date.now();
    try {
      this.startRecorder();
    } catch (e) {
      // Unsupported environment: give up for this page (the decision stays persisted for the session).
      this.ic.reportInternal('replay.record', e);
      this.mode = 'off';
      this.seg = this.prev = undefined;
    }
  }

  private startRecorder(): void {
    const stop = record({
      emit: (e: unknown, isCheckout?: boolean) => this.safe('emit', () => this.onRrweb(e as RrwebEvent, !!isCheckout)),
      ...rrwebPrivacyOptions(this.privacy),
      // rrweb observers report their own failures here instead of throwing into the page.
      errorHandler: (err: unknown) => {
        this.ic.reportInternal('rrweb', err);
        return true;
      },
    } as Parameters<typeof record>[0]);
    this.stopRecording = stop ?? undefined;
  }

  private stopRrweb(): void {
    const stop = this.stopRecording;
    this.stopRecording = undefined;
    if (stop) this.safe('rrweb.stop', stop);
  }

  /** New FullSnapshot (rrweb emits Meta with isCheckout=true, then FullSnapshot). */
  private checkout(): void {
    this.lastCheckout = Date.now();
    this.safe('checkout', () => record.takeFullSnapshot(true));
  }

  // --- recording --------------------------------------------------------------------------------

  private newSegment(): Segment {
    const v = this.ic.view();
    return { sessionId: this.sessionId, events: [], bytes: 0, errors: 0, rage: 0, viewId: v?.id, viewName: v?.name };
  }

  private onRrweb(e: RrwebEvent, isCheckout: boolean): void {
    if (this.mode === 'off' || !e || typeof e !== 'object') return;
    if (isCheckout) {
      this.lastCheckout = Date.now();
      if (this.mode === 'buffer') {
        this.prev = this.seg;
        this.seg = undefined;
      } else {
        this.flushSegment(); // every segment of a session after a checkout starts with a FullSnapshot
      }
    }
    const json = JSON.stringify(e);
    const d = e.data;
    const inc = e.type === INCREMENTAL && d;
    const input = !!inc && d.source === SRC_INPUT && d.id !== this.lastInputId;
    if (inc && d.source === SRC_INPUT) this.lastInputId = d.id;
    const ev: Ev = {
      json,
      ts: typeof e.timestamp === 'number' ? e.timestamp : Date.now(),
      full: e.type === FULL_SNAPSHOT,
      click: !!inc && d.source === SRC_MOUSE && d.type === MOUSE_CLICK,
      input,
    };
    const seg = (this.seg ??= this.newSegment());
    seg.events.push(ev);
    seg.bytes += json.length + 1;

    if (this.mode === 'session') {
      if (seg.bytes >= SEGMENT_BYTES) this.flushSegment();
      else if (!this.segTimer) this.segTimer = setTimeout(() => this.safe('segment.timer', () => this.flushSegment()), SEGMENT_MS);
    }
    // Lazy checkout (like rrweb's checkoutEveryNms): only while events flow; deferred out of rrweb's emit.
    const every = this.mode === 'buffer' ? BUFFER_MS : CHECKOUT_MS;
    if (!this.checkoutPending && ev.ts - this.lastCheckout >= every) {
      this.checkoutPending = true;
      setTimeout(() => {
        this.checkoutPending = false;
        if (this.mode !== 'off' && this.stopRecording) this.checkout();
      }, 0);
    }
  }

  private onRumEvent(e: RumEvent): void {
    if (this.mode === 'off' || (e.session_id && e.session_id !== this.sessionId)) return;
    if (e.type === 'ERROR') {
      (this.seg ??= this.newSegment()).errors++;
      if (this.mode === 'buffer') this.toSession(true);
    } else if (e.type === 'ACTION' && e.action?.rage) {
      (this.seg ??= this.newSegment()).rage++;
    }
  }

  /**
   * Another tab of this session switched the shared decision to `session` (an ERROR there; `storage` events only come from
   * other tabs): this buffering tab sends its window too and keeps recording, like on its own ERROR.
   */
  private onStorage(e: StorageEvent): void {
    if (this.mode === 'buffer' && e.key === KEY_PREFIX + this.sessionId && this.store.get(this.sessionId)?.m === 'session') this.toSession(false);
  }

  /**
   * Buffer mode → session: send the last ≥ 60 s (each window starts with a FullSnapshot) and keep recording this session.
   * `persist`: our own ERROR writes the shared decision; a decision read from another tab is already stored.
   */
  private toSession(persist: boolean): void {
    this.mode = 'session';
    if (persist)
      this.safe('store', () => {
        const cur = this.store.get(this.sessionId);
        this.store.set(this.sessionId, { m: 'session', n: cur?.n ?? 0 });
      });
    const segs = [this.prev, this.seg];
    this.prev = this.seg = undefined;
    for (const s of segs) if (s?.events.length) for (const part of split(s, SEGMENT_BYTES)) this.send(part, false);
  }

  // --- flushing ---------------------------------------------------------------------------------

  private clearSegTimer(): void {
    if (this.segTimer) clearTimeout(this.segTimer);
    this.segTimer = undefined;
  }

  /** Send the open session-mode segment (async, gzip). */
  flushSegment(): Promise<void> {
    this.clearSegTimer();
    if (this.mode !== 'session') return Promise.resolve(); // buffer mode: the window stays in memory
    const s = this.seg;
    this.seg = undefined;
    if (!s?.events.length) return Promise.resolve();
    return this.send(s, false);
  }

  /** Page hide: send the open segment now with fetch keepalive, split under the 64 KB keepalive budget. */
  flushKeepalive(): void {
    if (this.mode !== 'session') return;
    this.clearSegTimer();
    const s = this.seg;
    this.seg = undefined;
    if (!s?.events.length) return;
    for (const part of split(s, KEEPALIVE_LIMIT - 2048)) void this.send(part, true);
  }

  private send(s: Segment, keepalive: boolean): Promise<void> {
    // Sealed now: read-increment-write of the shared counter right before serializing (other tabs use it too).
    const n = this.store.takeSegment(s.sessionId, s.sessionId === this.sessionId ? this.mode : 'session');
    const body = this.serialize(s, n);
    if (keepalive) {
      this.transport.sendNow(body);
      return Promise.resolve();
    }
    return this.transport.enqueue(body);
  }

  serialize(s: Segment, n: number): string {
    const evs = s.events;
    const stats: SegmentStats = { clicks: 0, errors: s.errors, rage_clicks: s.rage, inputs: 0 };
    let full = false;
    for (const e of evs) {
      if (e.click) stats.clicks++;
      if (e.input) stats.inputs++;
      if (e.full) full = true;
    }
    const c = this.ic.config;
    const head: Omit<ReplaySegmentPayload, 'events'> = {
      resource: { service: c.service, env: c.environment, version: c.version, platform: 'web', sdk_name: SDK_NAME, sdk_version: SDK_VERSION },
      session_id: s.sessionId,
      segment: n,
      start_ts: evs[0]!.ts,
      end_ts: evs[evs.length - 1]!.ts,
      format: 'rrweb',
      has_full_snapshot: full,
      view_id: s.viewId,
      view_name: s.viewName,
      user_id: this.ic.userId() ?? '',
      anonymous_id: this.ic.anonymousId(),
      stats,
    };
    // Events were stringified once when recorded; splice them in.
    return JSON.stringify(head).slice(0, -1) + ',"events":[' + evs.map((e) => e.json).join(',') + ']}';
  }

  // --- teardown ---------------------------------------------------------------------------------

  /** Stop recording, send what is open and release listeners. */
  async stop(): Promise<void> {
    this.stopRrweb();
    for (const f of this.teardown.splice(0)) this.safe('teardown', f);
    await this.flushSegment().catch(() => undefined);
    this.mode = 'off';
    this.seg = this.prev = undefined;
    this.transport.close();
  }

  private safe<R>(where: string, fn: () => R): R | undefined {
    try {
      return fn();
    } catch (e) {
      this.ic.reportInternal(`replay.${where}`, e);
      return undefined;
    }
  }
}

/** Split a segment at event boundaries so each part stays under `limit` bytes; RUM counters go to the last part. */
export function split(s: Segment, limit: number): Segment[] {
  if (s.bytes <= limit) return [s];
  const out: Segment[] = [];
  let cur: Segment | undefined;
  for (const e of s.events) {
    if (!cur || (cur.events.length && cur.bytes + e.json.length + 1 > limit)) {
      cur = { ...s, events: [], bytes: 0, errors: 0, rage: 0 };
      out.push(cur);
    }
    cur.events.push(e);
    cur.bytes += e.json.length + 1;
  }
  const last = out[out.length - 1]!;
  last.errors = s.errors;
  last.rage = s.rage;
  return out;
}
