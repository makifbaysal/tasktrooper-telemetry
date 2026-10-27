import { context as otelContext, trace, type Context, type SpanContext } from '@opentelemetry/api';
import type { RPCMetadata } from '@opentelemetry/core';
import type { Span } from '@opentelemetry/sdk-trace';
import { normalizeRoute } from './route';
import { recordError } from './errors';

/**
 * The entry point (HTTP route or consumer process) of a unit of work. Created by the trooper span processor for
 * every local-root SERVER / CONSUMER span; every span started below it is linked to it, so its route can be copied
 * onto them as `trooper.entry.route` / `trooper.entry.method`, even when the route template is resolved later
 * (framework routers match after the HTTP span started).
 */
export interface Entry {
  readonly root: Span;
  readonly kind: 'server' | 'consumer';
  /** Explicit route (setRoute) or consumer process name. */
  route?: string;
  method?: string;
  /** Lazily consulted route resolver installed by framework helpers. */
  resolve?: () => string | undefined;
  /** Route metadata that OTel framework instrumentations (express, koa, nestjs …) update in the context. */
  rpc?: RPCMetadata;
  /** Delivery attempt of the consumed message. */
  attempt?: number;
}

const MAX_OPEN = 100_000;
const open = new Map<string, Entry>();

export function spanKey(sc: SpanContext): string {
  return sc.traceId + sc.spanId;
}

/** Links an open span to its entry. Bounded: beyond MAX_OPEN spans (leaked, never ended) nothing is tracked. */
export function linkSpan(key: string, e: Entry): void {
  if (open.size < MAX_OPEN) open.set(key, e);
}

export function unlinkSpan(key: string): void {
  open.delete(key);
}

export function entryOfKey(key: string): Entry | undefined {
  return open.get(key);
}

/** Number of open spans currently tracked (diagnostics / tests). */
export function trackedSpans(): number {
  return open.size;
}

/** Entry of the active span in `ctx` (default: the active context). */
export function currentEntry(ctx: Context = otelContext.active()): Entry | undefined {
  const sc = trace.getSpanContext(ctx);
  return sc ? open.get(spanKey(sc)) : undefined;
}

function safeResolve(f: () => string | undefined): string | undefined {
  try {
    return f();
  } catch (err) {
    recordError('resolver', err);
    return undefined;
  }
}

/** Raw (un-normalized) server route template known right now, and whether it came from trooper itself. */
export function rawRoute(e: Entry): { route?: string; own: boolean } {
  if (e.kind === 'consumer') return { route: e.route, own: true };
  if (e.route) return { route: e.route, own: true };
  if (e.resolve) {
    const r = safeResolve(e.resolve);
    if (r) return { route: r, own: true };
  }
  const attr = e.root.attributes['http.route'];
  if (typeof attr === 'string' && attr) return { route: attr, own: false };
  if (e.rpc && 'route' in e.rpc && typeof e.rpc.route === 'string' && e.rpc.route) return { route: e.rpc.route, own: false };
  return { own: false };
}

/** Normalized entry route (server route template or consumer process name). */
export function entryRoute(e: Entry): string | undefined {
  const { route } = rawRoute(e);
  if (!route) return undefined;
  return e.kind === 'consumer' ? route : normalizeRoute(route);
}
