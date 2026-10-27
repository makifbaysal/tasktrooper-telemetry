import { context as otelContext, SpanStatusCode, trace, type Attributes, type Context } from '@opentelemetry/api';
import { ATTR_CONSUMER_ATTEMPT, ATTR_CONSUMER_DLQ, ATTR_PROCESS } from './attrs';
import { nextAttemptAfter } from './attempt';
import { currentEntry, entryOfKey, spanKey } from './entry';
import { recordError } from './errors';

/**
 * Sets the route template of the current request (e.g. `/users/:id`). Use it when no framework helper applies.
 * Becomes `trooper.route` on the server span and `trooper.entry.route` on every span of the request. Never throws.
 */
export function setRoute(route: string, ctx: Context = otelContext.active()): void {
  try {
    const e = currentEntry(ctx);
    if (e && e.kind === 'server' && route) e.route = route;
  } catch (err) {
    recordError('api', err);
  }
}

/**
 * Installs a lazy route resolver for the current request (framework helpers use it: the router matches after the
 * HTTP span started). Explicit `setRoute` wins.
 */
export function setRouteResolver(resolve: () => string | undefined, ctx: Context = otelContext.active()): void {
  try {
    const e = currentEntry(ctx);
    if (e && e.kind === 'server') e.resolve = resolve;
  } catch (err) {
    recordError('api', err);
  }
}

/** Names the consumer process of the current message (`trooper.process`, entry route of the message). */
export function setProcessName(name: string, ctx: Context = otelContext.active()): void {
  try {
    const e = currentEntry(ctx);
    if (e && e.kind === 'consumer' && name) {
      e.route = name;
      e.root.setAttribute(ATTR_PROCESS, name);
    }
  } catch (err) {
    recordError('api', err);
  }
}

/** Marks the current consumer message as sent to a dead letter queue (`trooper.consumer.dlq=true`). */
export function markDlq(ctx: Context = otelContext.active()): void {
  try {
    const e = currentEntry(ctx);
    if (e && e.kind === 'consumer') e.root.setAttribute(ATTR_CONSUMER_DLQ, true);
    else trace.getSpan(ctx)?.setAttribute(ATTR_CONSUMER_DLQ, true);
  } catch (err) {
    recordError('api', err);
  }
}

/** Delivery attempt of the message being processed in `ctx` (1 = first delivery), when known. */
export function currentAttempt(ctx: Context = otelContext.active()): number | undefined {
  try {
    return currentEntry(ctx)?.attempt;
  } catch (err) {
    recordError('api', err);
    return undefined;
  }
}

/** Attempt to write into `tt-attempt` when re-publishing the current message to a retry topic / queue. */
export function nextAttempt(ctx: Context = otelContext.active()): number {
  return nextAttemptAfter(currentAttempt(ctx));
}

/**
 * Records the delivery attempt on a consumer span (and its entry, so `currentAttempt()` / `nextAttempt()` see it).
 * Used by the messaging integrations; explicit values win over header-derived ones.
 */
export function setSpanAttempt(span: import('@opentelemetry/api').Span | undefined, attempt: number | undefined): void {
  if (!span || attempt === undefined || attempt < 1) return;
  try {
    span.setAttribute(ATTR_CONSUMER_ATTEMPT, attempt);
    const e = entryOfKey(spanKey(span.spanContext()));
    if (e) e.attempt = attempt;
  } catch (err) {
    recordError('api', err);
  }
}

/** Records `err` on the active span (exception event + error status). No-op without a span. Never throws. */
export function captureError(err: unknown, attributes?: Attributes, ctx: Context = otelContext.active()): void {
  if (err == null) return;
  try {
    const span = trace.getSpan(ctx);
    if (!span || !span.isRecording()) return;
    const e = err instanceof Error ? err : new Error(String(err));
    span.recordException(e);
    if (attributes) span.setAttributes(attributes);
    span.setStatus({ code: SpanStatusCode.ERROR, message: e.message });
  } catch (e) {
    recordError('capture', e);
  }
}
