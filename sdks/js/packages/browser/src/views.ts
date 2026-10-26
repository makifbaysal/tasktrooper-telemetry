import { normalizePath, uuid, type Client } from '@trooper/core';
import type { Activity } from './activity';
import { perfNow } from './env';

export interface View {
  id: string;
  name: string;
  /** Path the view was started for (pathname + hash route). */
  path: string;
  url: string;
  /** true while the name is the SDK's normalized path (a router integration may still rename it). */
  auto: boolean;
  initial: boolean;
  start: number;
  t0: number;
  referrer: string;
  loading?: number;
  /** VIEW_START already emitted. */
  emitted?: boolean;
}

/** Route path of the page: the hash route for hash routers (`/#/users/1` → `/users/1`), else the pathname. */
export const currentPath = (): string => {
  const h = location.hash;
  return h.startsWith('#/') ? h.slice(1).split('?')[0]! : location.pathname;
};

const SETTLE_IDLE_MS = 100;
const SETTLE_MAX_MS = 10000;
/** Emit the initial VIEW_START without loading time if the load event has not fired by then. */
const INITIAL_MAX_MS = 30000;

/**
 * Page/route views. VIEW_START carries `loading_time_ms` (contracts.md §9), so it is emitted once the page load /
 * SPA route settle is measured — or earlier when the view ends or the page is hidden. Its `ts_ms` is the
 * navigation time. VIEW_END carries `time_spent_ms`.
 * View name priority: router integration / setView() (route template) > normalized path.
 */
export class Views {
  cur: View | undefined;
  initial: View | undefined;
  /** A router integration owns view changes: History API changes no longer start views by themselves. */
  routerMode = false;
  private lastUrl = '';

  constructor(
    private readonly client: Client,
    private readonly activity: Activity,
  ) {}

  start(name: string, auto: boolean, initial = false, path = currentPath()): View {
    this.end();
    const t = Date.now();
    const url = location.href;
    const referrer = initial ? document.referrer : this.lastUrl;
    const v: View = { id: uuid(), name, path, url, referrer, auto, initial, start: t, t0: perfNow() };
    this.cur = v;
    if (initial) this.initial = v;
    this.client.view = { id: v.id, name };
    this.lastUrl = url;
    if (initial) this.measureInitial(v);
    else this.measureSettle(v);
    return v;
  }

  end(): void {
    const v = this.cur;
    if (!v) return;
    this.cur = undefined;
    this.emitStart(v);
    this.client.capture({
      type: 'VIEW_END',
      view_id: v.id,
      view_name: v.name,
      view: { url: v.url, time_spent_ms: Math.max(0, Date.now() - v.start), is_initial: v.initial },
    });
  }

  /** Emit VIEW_START for the current view now (page hidden: the load may never be observed). */
  emitPending(): void {
    if (this.cur) this.emitStart(this.cur);
  }

  private emitStart(v: View): void {
    if (v.emitted) return;
    v.emitted = true;
    this.client.capture({
      type: 'VIEW_START',
      ts_ms: v.start,
      view_id: v.id,
      view_name: v.name,
      view: { url: v.url, referrer: v.referrer, is_initial: v.initial, loading_time_ms: v.loading },
    });
  }

  /** History API / popstate / hashchange. */
  onLocationChange = (): void => {
    if (this.routerMode) return;
    const p = currentPath();
    if (!this.cur || p !== this.cur.path) this.start(normalizePath(p), true);
  };

  /**
   * Name the current view (route template) or start a new one.
   * `router`: called by a framework integration on every route change; switches to router mode.
   * `path`: the router's own location path (memory routers do not touch window.location).
   */
  setName(name: string, router: boolean, path?: string): void {
    if (router) this.routerMode = true;
    const v = this.cur;
    const p = path ?? currentPath();
    if (v && v.path === p && (v.auto || router)) {
      if (v.name !== name || v.auto) this.rename(v, name);
      return;
    }
    if (v && !router && v.name === name) return;
    this.start(name, false, false, p);
  }

  private rename(v: View, name: string): void {
    v.name = name;
    v.auto = false;
    if (this.cur === v) this.client.view = { id: v.id, name };
    this.client.patchBuffered((e) => {
      if (e.view_id === v.id) e.view_name = name;
    });
  }

  private setLoading(v: View, ms: number | undefined): void {
    if (v.loading !== undefined || v.emitted) return;
    if (ms !== undefined) v.loading = Math.max(0, Math.round(ms));
    this.emitStart(v);
  }

  /** Initial page: navigation timing load event end. */
  private measureInitial(v: View): void {
    const read = (): boolean => {
      const nav = performance.getEntriesByType?.('navigation')[0] as PerformanceNavigationTiming | undefined;
      if (nav && nav.loadEventEnd > 0) {
        this.setLoading(v, nav.loadEventEnd);
        return true;
      }
      return false;
    };
    if (typeof performance === 'undefined') return this.setLoading(v, undefined);
    if (read()) return;
    setTimeout(() => this.setLoading(v, undefined), INITIAL_MAX_MS);
    if (document.readyState === 'complete') {
      setTimeout(() => read() || this.setLoading(v, perfNow()), 0);
    } else {
      addEventListener('load', () => setTimeout(() => read() || this.setLoading(v, perfNow()), 0), { once: true });
    }
  }

  /** SPA route: time until no DOM mutation for 100 ms and no request in flight (cap 10 s). */
  private measureSettle(v: View): void {
    const check = () => {
      if (this.cur !== v || v.emitted) return;
      const t = perfNow();
      const lastActivity = Math.max(this.activity.last, v.t0);
      if (this.activity.pending === 0 && t - lastActivity >= SETTLE_IDLE_MS) this.setLoading(v, lastActivity - v.t0);
      else if (t - v.t0 >= SETTLE_MAX_MS) this.setLoading(v, SETTLE_MAX_MS);
      else setTimeout(check, 50);
    };
    setTimeout(check, SETTLE_IDLE_MS);
  }
}
