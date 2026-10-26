import {
  Client,
  instrumentFetch,
  instrumentXhr,
  normalizePath,
  parseDsn,
  reportInternal,
  safe,
  toStringMap,
  uuid,
  type CoreOptions,
  type ErrorPayload,
  type PropagationTarget,
} from '@trooper/core';
import { onCLS, onFCP, onINP, onLCP, onTTFB, type Metric } from 'web-vitals';
import { Actions } from './actions';
import { Activity } from './activity';
import { connection, detectEnv, localGet, localSessionStore, localSet } from './env';
import { firstReport, toErrorPayload } from './errors';
import { IdbOfflineStore } from './idb';
import { BrowserTransport } from './transport';
import { currentPath, Views } from './views';

export const SDK_NAME = '@trooper/browser';
export const SDK_VERSION = '0.1.0';

export interface BrowserOptions extends CoreOptions {
  /**
   * URLs that receive a W3C `traceparent` header (string = URL prefix or origin, RegExp = tested on the absolute URL).
   * Matching requests also get `baggage: tt.svc=<app>`.
   * Default: same-origin requests only. Cross-origin targets must allow the `traceparent` and `baggage` headers via CORS.
   */
  tracePropagationTargets?: PropagationTarget[];
  /** Page views (initial + SPA History API). Default true. */
  trackViews?: boolean;
  /** window.onerror + unhandledrejection. Default true. */
  trackErrors?: boolean;
  /** Also report `console.error(...)` calls. Default false (opt-in). */
  captureConsoleErrors?: boolean;
  /** fetch + XHR RESOURCE events. Default true. */
  trackNetwork?: boolean;
  /** Clicks, rage and dead clicks. Default true. */
  trackActions?: boolean;
  /** LCP, INP, CLS, FCP, TTFB. Default true. */
  trackVitals?: boolean;
  /** Persist session/anonymous id in localStorage and the offline queue in IndexedDB. Default true. */
  persist?: boolean;
}

export interface CaptureErrorOptions {
  /** ErrorPayload.source; default "source". */
  source?: string;
  /** Unhandled error (web) — default false for manual captures. */
  isCrash?: boolean;
  attributes?: Record<string, unknown>;
}

export class BrowserSdk {
  readonly client: Client;
  readonly views: Views;
  readonly activity = new Activity();
  private actions: Actions | undefined;
  private teardown: (() => void)[] = [];

  constructor(readonly options: BrowserOptions) {
    const persist = options.persist !== false;
    const env = detectEnv();
    let anon = persist ? localGet('tt_anon') : undefined;
    if (!anon) {
      anon = uuid();
      if (persist) localSet('tt_anon', anon);
    }
    const dsn = parseDsn(options.dsn);
    const client = new Client({
      ...options,
      platform: 'PLATFORM_WEB',
      sdk: { name: SDK_NAME, version: SDK_VERSION },
      ...env,
      anonymousId: anon,
      sessionStore: persist ? localSessionStore() : undefined,
      offlineStore: persist ? new IdbOfflineStore() : undefined,
      // Created before fetch is instrumented, so it captures the unpatched fetch.
      transport: new BrowserTransport(dsn.rumEndpoint, dsn.key),
      getConnection: connection,
      isOnline: () => navigator.onLine !== false,
      // A rotated session (30 min idle / 4 h) starts a fresh instance of the current view.
      onSessionStart: (_s, first) => {
        const v = this.views?.cur;
        if (!first && v) this.views.start(v.name, v.auto);
      },
    });
    this.client = client;
    this.views = new Views(client, this.activity);
  }

  start(): void {
    const o = this.options;
    const c = this.client;
    this.activity.start();
    this.teardown.push(() => this.activity.stop());

    if (o.trackViews !== false) {
      this.views.start(normalizeInitial(), true, true);
      this.patchHistory();
    }

    if (o.trackErrors !== false) this.instrumentErrors();
    if (o.captureConsoleErrors) this.instrumentConsole();

    if (o.trackNetwork !== false) {
      const net = {
        capture: (e: Parameters<Client['capture']>[0]) => c.capture(e),
        tracePropagationTargets: o.tracePropagationTargets,
        serviceName: o.app,
        currentOrigin: () => location.origin,
        ignoreUrls: [c.dsn.origin + '/v1/'],
        onStart: this.activity.requestStart,
        onEnd: this.activity.requestEnd,
      };
      this.teardown.push(instrumentFetch(net), instrumentXhr(net));
    }

    if (o.trackActions !== false) {
      this.actions = new Actions(c, this.activity);
      this.actions.start();
      this.teardown.push(() => this.actions?.stop());
    }

    if (o.trackVitals !== false) this.instrumentVitals();

    // Flush on hide (beacon). Listen on window in the bubble phase so web-vitals' document listeners
    // (which report INP/CLS/LCP on hide) run first.
    const onVisibility = () => {
      if (document.visibilityState === 'hidden') {
        this.actions?.flushAll(false);
        this.views.emitPending();
        void c.flush({ beacon: true });
      }
    };
    const onPageHide = () => {
      this.actions?.flushAll(true);
      this.views.end();
      void c.flush({ beacon: true });
    };
    const onPageShow = (e: PageTransitionEvent) => {
      if (e.persisted && this.options.trackViews !== false) this.views.start(this.views.initial?.name ?? normalizeInitial(), true);
    };
    const onOnline = () => void c.drain();
    this.listen(window, 'visibilitychange', onVisibility);
    this.listen(window, 'pagehide', onPageHide);
    this.listen(window, 'pageshow', onPageShow as EventListener);
    this.listen(window, 'online', onOnline);
    // Deliver batches queued in IndexedDB by a previous page load.
    setTimeout(() => void c.drain(), 1000);
  }

  private listen(t: EventTarget, type: string, fn: EventListener, capture = false): void {
    const wrapped: EventListener = (e) => safe(type, () => fn(e));
    t.addEventListener(type, wrapped, capture);
    this.teardown.push(() => t.removeEventListener(type, wrapped, capture));
  }

  private patchHistory(): void {
    const h = history;
    const onChange = () => {
      this.activity.ping();
      safe('views.location', this.views.onLocationChange);
    };
    for (const m of ['pushState', 'replaceState'] as const) {
      const orig = h[m];
      const patched = function (this: History, ...args: Parameters<History['pushState']>) {
        const r = orig.apply(this, args);
        onChange();
        return r;
      };
      h[m] = patched;
      this.teardown.push(() => {
        if (h[m] === patched) h[m] = orig;
      });
    }
    this.listen(window, 'popstate', onChange);
    this.listen(window, 'hashchange', onChange);
  }

  private instrumentErrors(): void {
    this.listen(window, 'error', (e) => {
      const ev = e as ErrorEvent;
      // Resource load errors (img/script 404) are Events without `message`; they are not JS errors.
      if (!('message' in ev) || (!ev.error && !ev.message)) return;
      this.captureError(ev.error ?? ev.message, { source: 'source', isCrash: true });
    });
    this.listen(window, 'unhandledrejection', (e) => {
      this.captureError((e as PromiseRejectionEvent).reason, { source: 'promise', isCrash: true });
    });
  }

  private instrumentConsole(): void {
    const con = console;
    const orig = con.error;
    const patched = (...args: unknown[]) => {
      safe('console.error', () => {
        const err = args.find((a) => a instanceof Error);
        const msg = args.map((a) => (typeof a === 'string' ? a : a instanceof Error ? a.message : safeJson(a))).join(' ');
        this.captureError(err ?? msg, { source: 'console', isCrash: false });
      });
      return orig.apply(con, args);
    };
    con.error = patched;
    this.teardown.push(() => {
      if (con.error === patched) con.error = orig;
    });
  }

  private instrumentVitals(): void {
    const report = (m: Metric) =>
      safe('vital', () => {
        const v = this.views.initial;
        this.client.capture({
          type: 'VITAL',
          view_id: v?.id ?? this.client.view?.id,
          view_name: v?.name ?? this.client.view?.name,
          vital: { name: m.name, value: m.name === 'CLS' ? Math.round(m.value * 10000) / 10000 : Math.round(m.value), rating: m.rating },
        });
      });
    for (const on of [onLCP, onINP, onCLS, onFCP, onTTFB]) safe('web-vitals', () => on(report));
  }

  captureError(err: unknown, opts: CaptureErrorOptions = {}): void {
    if (!firstReport(err)) return;
    const error: ErrorPayload = { ...toErrorPayload(err), source: opts.source ?? 'source', is_crash: !!opts.isCrash };
    this.client.capture({ type: 'ERROR', error, attributes: toStringMap(opts.attributes, 4000) });
  }

  close(): Promise<void> {
    this.views.end();
    for (const f of this.teardown.splice(0).reverse()) safe('teardown', f);
    return this.client.close();
  }
}

const normalizeInitial = (): string => normalizePath(currentPath());

const safeJson = (v: unknown): string => {
  try {
    return JSON.stringify(v) ?? String(v);
  } catch {
    return String(v);
  }
};

// ---------------------------------------------------------------------------------------------
// Public singleton API — every function is safe to call before init / during SSR and never throws.
// ---------------------------------------------------------------------------------------------

let sdk: BrowserSdk | undefined;

/** Initialize the SDK once (subsequent calls return the existing instance). No-op outside a browser (SSR). */
export function init(options: BrowserOptions): BrowserSdk | undefined {
  if (sdk) return sdk;
  if (typeof window === 'undefined' || typeof document === 'undefined') return undefined;
  try {
    const s = new BrowserSdk(options);
    sdk = s;
    if (s.client.enabled) s.start();
    return s;
  } catch (e) {
    reportInternal('init', e);
    if (options?.debug) console.warn('[trooper] init failed', e);
    sdk = undefined;
    return undefined;
  }
}

/** The active SDK instance (for framework integrations). */
export const getSdk = (): BrowserSdk | undefined => sdk;

/** Attach a user id (+ optional opt-in traits) to all following events. PII in traits is the app's responsibility. */
export function identify(userId: string, traits?: Record<string, unknown>): void {
  safe('identify', () => sdk?.client.setUser(userId, traits));
}

/** Forget the user (logout). Also starts a new session. */
export function clearUser(): void {
  safe('clearUser', () => {
    sdk?.client.setUser(undefined);
    sdk?.client.sessions.reset();
  });
}

/** Custom event (CUSTOM). `props` values are stringified; `value` is an optional numeric measure. */
export function track(name: string, props?: Record<string, unknown>, value?: number): void {
  safe('track', () => sdk?.client.capture({ type: 'CUSTOM', custom: { name: String(name), props: toStringMap(props), value } }));
}

/** Report a handled error. */
export function captureError(err: unknown, opts?: CaptureErrorOptions): void {
  safe('captureError', () => sdk?.captureError(err, opts));
}

export interface SetViewOptions {
  /** Set by router integrations: the router becomes the source of truth for view changes. */
  router?: boolean;
  /** The router's current path (pathname). Defaults to window.location. */
  path?: string;
}

/**
 * Name the current view with a route template (e.g. "/products/:id"), or start a new view.
 * If the current view was auto-named for the same URL, it is renamed (buffered events included).
 */
export function setView(name: string, opts?: SetViewOptions): void {
  safe('setView', () => sdk?.views.setName(String(name), !!opts?.router, opts?.path));
}

/** Send buffered events now. */
export function flush(): Promise<void> {
  return sdk ? sdk.client.flush().catch(() => undefined) : Promise.resolve();
}

/** Flush, remove all instrumentation and reset the singleton. */
export async function close(): Promise<void> {
  const s = sdk;
  sdk = undefined;
  if (s) await s.close().catch((e) => reportInternal('close', e));
}

export type { BeforeSend, CoreOptions, PropagationTarget, RumBatch, RumEvent } from '@trooper/core';
export { normalizePath, stripQueryValues } from '@trooper/core';
