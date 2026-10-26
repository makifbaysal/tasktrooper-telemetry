import { parseDsn, type Dsn } from './dsn';
import { hashToUnit, uuid } from './ids';
import { MemoryOfflineStore, type OfflineStore } from './offline';
import { SessionManager, type SessionState, type SessionStore } from './session';
import { backoffDelay, FetchTransport, KEEPALIVE_LIMIT, type SendResult, type Transport } from './transport';
import type { Browser, Device, EventType, Os, Platform, Resource, RumEvent, Sdk, StringMap, User } from './types';
import { stripQueryValues } from './url';
import { now, reportInternal, safe, setDebug, toStringMap } from './util';

export type BeforeSend = (event: RumEvent) => RumEvent | null | false | undefined | void;

/** Options shared by every client SDK's `init()`. */
export interface CoreOptions {
  /** `https://<ingest-key>@<host>/<project_id>` */
  dsn: string;
  /** Client app (service) name, e.g. "shop-web". */
  app: string;
  version?: string;
  environment?: string;
  /** Fraction of sessions to record (0..1, default 1). Decided once per session. */
  sampleRate?: number;
  /**
   * Last chance to modify or drop an event (return `null`/`false` to drop). URLs are already scrubbed.
   * If the hook throws, the event is dropped (fail closed: it may exist to remove PII).
   */
  beforeSend?: BeforeSend;
  /** Max events per batch (default and maximum 100). */
  maxBatchSize?: number;
  /** Max time an event waits before being sent (default 5000 ms). */
  flushIntervalMs?: number;
  /** Extra resource attributes. */
  attributes?: Record<string, string>;
  /** Set false to turn the SDK into a no-op (e.g. in tests). */
  enabled?: boolean;
  /** Log internal SDK errors to the console. */
  debug?: boolean;
}

export interface ClientConfig extends CoreOptions {
  platform: Platform;
  sdk: Sdk;
  device?: Device;
  os?: Os;
  browser?: Browser;
  transport?: Transport;
  offlineStore?: OfflineStore;
  sessionStore?: SessionStore;
  anonymousId?: string;
  getConnection?: () => string | undefined;
  isOnline?: () => boolean;
  /** Called right after SESSION_START is recorded (`first` = first session of this client). */
  onSessionStart?: (session: SessionState, first: boolean) => void;
}

/** Input for `capture`: the event type, a payload and optional overrides of the common fields. */
export type EventInput = Partial<Omit<RumEvent, 'type'>> & { type: EventType };

export interface ViewRef {
  id: string;
  name: string;
}

export const MAX_BATCH = 100;

/**
 * Platform-neutral RUM client: enriches events with session/view/user, samples, scrubs, batches
 * (≤100 events or 5 s) and delivers with retry/backoff and an offline queue.
 */
export class Client {
  readonly dsn: Dsn;
  readonly resource: Resource;
  readonly sessions: SessionManager;
  readonly store: OfflineStore;
  readonly transport: Transport;
  anonymousId: string;
  user: User | undefined;
  view: ViewRef | undefined;
  readonly stats = { sent: 0, dropped: 0, retried: 0, sampledOut: 0 };

  private buffer: RumEvent[] = [];
  private flushTimer: ReturnType<typeof setTimeout> | undefined;
  private retryTimer: ReturnType<typeof setTimeout> | undefined;
  private attempt = 0;
  private drainP: Promise<void> | undefined;
  private sampledFor = '';
  private sampled = true;
  private hadSession = false;
  private closed = false;
  private readonly maxBatch: number;
  private readonly interval: number;

  constructor(readonly config: ClientConfig) {
    setDebug(!!config.debug);
    this.dsn = parseDsn(config.dsn);
    this.maxBatch = Math.max(1, Math.min(MAX_BATCH, config.maxBatchSize ?? MAX_BATCH));
    this.interval = config.flushIntervalMs ?? 5000;
    this.resource = {
      app: config.app,
      version: config.version,
      environment: config.environment,
      platform: config.platform,
      sdk: config.sdk,
      device: config.device,
      os: config.os,
      browser: config.browser,
      attributes: config.attributes,
    };
    this.sessions = new SessionManager(config.sessionStore);
    this.store = config.offlineStore ?? new MemoryOfflineStore();
    this.transport = config.transport ?? new FetchTransport({ endpoint: this.dsn.rumEndpoint, key: this.dsn.key });
    this.anonymousId = config.anonymousId || uuid();
    if (config.enabled === false) this.closed = true;
  }

  get enabled(): boolean {
    return !this.closed;
  }

  /** Session id, creating a session if needed. */
  sessionId(): string {
    return this.touch(now()).id;
  }

  private touch(t: number): SessionState {
    const { session, isNew } = this.sessions.touch(t);
    if (session.id !== this.sampledFor) {
      this.sampledFor = session.id;
      const rate = this.config.sampleRate ?? 1;
      this.sampled = rate >= 1 || hashToUnit(session.id) < rate;
    }
    if (isNew) {
      const first = !this.hadSession;
      this.hadSession = true;
      if (this.sampled) this.emit(this.build({ type: 'SESSION_START', ts_ms: session.start }, session));
      safe('onSessionStart', () => this.config.onSessionStart?.(session, first));
    }
    return session;
  }

  private build(input: EventInput, session: SessionState): RumEvent {
    const ev: RumEvent = {
      ...input,
      type: input.type,
      ts_ms: input.ts_ms ?? now(),
      session_id: session.id,
      view_id: 'view_id' in input ? input.view_id : this.view?.id,
      view_name: 'view_name' in input ? input.view_name : this.view?.name,
      anonymous_id: this.anonymousId,
      user: this.user,
      connection: input.connection ?? safe('connection', () => this.config.getConnection?.()) ?? undefined,
    };
    if (ev.view) ev.view = { ...ev.view, url: scrub(ev.view.url), referrer: scrub(ev.view.referrer) };
    if (ev.resource) ev.resource = { ...ev.resource, url: scrub(ev.resource.url) };
    return ev;
  }

  /** Record an event. Returns the enqueued event (mutable until flushed) or undefined when dropped. */
  capture(input: EventInput): RumEvent | undefined {
    if (this.closed) return undefined;
    try {
      const session = this.touch(input.ts_ms ?? now());
      if (!this.sampled) {
        this.stats.sampledOut++;
        return undefined;
      }
      return this.emit(this.build(input, session));
    } catch (e) {
      reportInternal('capture', e);
      return undefined;
    }
  }

  /** beforeSend + enqueue. */
  private emit(input: RumEvent): RumEvent | undefined {
    let ev: RumEvent | undefined = input;
    if (this.config.beforeSend) {
      try {
        ev = this.config.beforeSend(input) || undefined;
      } catch (e) {
        reportInternal('beforeSend', e);
        ev = undefined;
      }
    }
    if (!ev) {
      this.stats.dropped++;
      return undefined;
    }
    this.enqueue(ev);
    return ev;
  }

  private enqueue(ev: RumEvent): void {
    this.buffer.push(ev);
    if (this.buffer.length >= this.maxBatch) void this.flush();
    else if (!this.flushTimer) this.flushTimer = setTimeout(() => void this.flush(), this.interval);
  }

  /** Apply `fn` to events still waiting in the buffer (e.g. rename a view after its route template is known). */
  patchBuffered(fn: (e: RumEvent) => void): void {
    for (const e of this.buffer) safe('patchBuffered', () => fn(e));
  }

  /** Number of events waiting in the buffer. */
  pending(): number {
    return this.buffer.length;
  }

  /**
   * Adopt ids owned by someone else (hybrid apps: the native SDK started first, or a persisted anonymous id loaded
   * asynchronously). Buffered events are rewritten so the whole batch is consistent.
   */
  adoptIdentity(ids: { sessionId?: string; sessionStart?: number; anonymousId?: string }): void {
    safe('adoptIdentity', () => {
      const t = now();
      if (ids.anonymousId && ids.anonymousId !== this.anonymousId) {
        const old = this.anonymousId;
        this.anonymousId = ids.anonymousId;
        this.patchBuffered((e) => {
          if (e.anonymous_id === old) e.anonymous_id = ids.anonymousId!;
        });
      }
      const cur = this.sessions.peek();
      if (ids.sessionId && ids.sessionId !== cur?.id) {
        this.sessions.adopt(ids.sessionId, ids.sessionStart ?? t, t);
        this.hadSession = true;
        if (cur) {
          this.patchBuffered((e) => {
            if (e.session_id === cur.id) e.session_id = ids.sessionId!;
          });
        }
      }
    });
  }

  setUser(id: string | undefined, traits?: Record<string, unknown>): void {
    this.user = id ? { id: String(id), traits: toStringMap(traits) } : undefined;
  }

  setAttributes(attrs: StringMap): void {
    this.resource.attributes = { ...this.resource.attributes, ...attrs };
  }

  /**
   * Send buffered events now. With `beacon: true` (page hide / app background) the transport is invoked
   * synchronously so the request leaves before the page is frozen.
   */
  flush(opts?: { beacon?: boolean }): Promise<void> {
    if (this.flushTimer) {
      clearTimeout(this.flushTimer);
      this.flushTimer = undefined;
    }
    const events = this.buffer.splice(0);
    const jobs: Promise<void>[] = [];
    for (let i = 0; i < events.length; i += this.maxBatch) {
      const chunk = events.slice(i, i + this.maxBatch);
      if (opts?.beacon) this.sendBeacon(chunk, jobs);
      else jobs.push(this.deliver(this.serialize(chunk)));
    }
    if (!events.length && !opts?.beacon) jobs.push(this.drain());
    return Promise.all(jobs).then(
      () => undefined,
      (e) => reportInternal('flush', e),
    );
  }

  private serialize(events: RumEvent[]): string {
    return JSON.stringify({ resource: this.resource, events });
  }

  private sendBeacon(events: RumEvent[], jobs: Promise<void>[]): void {
    const body = this.serialize(events);
    if (body.length > KEEPALIVE_LIMIT && events.length > 1) {
      const mid = events.length >> 1;
      this.sendBeacon(events.slice(0, mid), jobs);
      this.sendBeacon(events.slice(mid), jobs);
      return;
    }
    let p: Promise<SendResult>;
    try {
      p = this.transport.send(body, { beacon: true });
    } catch (e) {
      reportInternal('transport.beacon', e);
      p = Promise.resolve({ status: 'retry' });
    }
    jobs.push(
      p.then(async (r) => {
        if (r.status === 'retry') await this.store.push(body);
        else this.count(r);
      }),
    );
  }

  private count(r: SendResult): void {
    if (r.status === 'ok') this.stats.sent++;
    else if (r.status === 'drop') this.stats.dropped++;
  }

  private online(): boolean {
    return safe('isOnline', () => this.config.isOnline?.()) ?? true;
  }

  private async deliver(body: string): Promise<void> {
    if (this.retryTimer || !this.online()) {
      await this.store.push(body);
      return;
    }
    if (this.drainP) {
      // Keep FIFO order: queue behind the running drain, then drain again (it may have already passed the tail).
      await this.store.push(body);
      await this.drainP;
      return this.drain();
    }
    const r = await this.transport.send(body);
    this.count(r);
    if (r.status === 'ok') {
      this.attempt = 0;
      await this.drain();
    } else if (r.status === 'retry') {
      await this.store.push(body);
      this.scheduleRetry(r.retryAfterMs);
    }
  }

  /** Send queued (offline / failed) batches oldest-first until one fails. */
  drain(): Promise<void> {
    if (this.retryTimer || this.closed || !this.online()) return Promise.resolve();
    if (!this.drainP) {
      this.drainP = this.drainLoop().finally(() => {
        this.drainP = undefined;
      });
    }
    return this.drainP;
  }

  private async drainLoop(): Promise<void> {
    try {
      for (;;) {
        const body = await this.store.peek();
        if (body === undefined) break;
        const r = await this.transport.send(body);
        this.count(r);
        if (r.status === 'retry') {
          this.scheduleRetry(r.retryAfterMs);
          break;
        }
        if (r.status === 'ok') this.attempt = 0;
        await this.store.shift();
      }
    } catch (e) {
      reportInternal('drain', e);
    }
  }

  private scheduleRetry(retryAfterMs?: number): void {
    if (this.retryTimer || this.closed) return;
    this.stats.retried++;
    const delay = Math.max(retryAfterMs ?? 0, backoffDelay(this.attempt++));
    this.retryTimer = setTimeout(() => {
      this.retryTimer = undefined;
      void this.drain();
    }, delay);
  }

  /** Flush and stop. Further captures are ignored. */
  async close(): Promise<void> {
    if (this.closed) return;
    await this.flush();
    this.closed = true;
    if (this.retryTimer) clearTimeout(this.retryTimer);
    this.retryTimer = undefined;
  }
}

const scrub = (u: string | undefined): string | undefined => (u ? stripQueryValues(u) : u);
