import { SpanKind, trace, type AttributeValue, type Context } from '@opentelemetry/api';
import { getRPCMetadata } from '@opentelemetry/core';
import type { ReadableSpan, Span, SpanProcessor } from '@opentelemetry/sdk-trace';
import {
  ATTR_CALLER,
  ATTR_CONSUMER_ATTEMPT,
  ATTR_ENTRY_METHOD,
  ATTR_ENTRY_ROUTE,
  ATTR_PROCESS,
  ATTR_ROUTE,
  ATTR_SAMPLE_RATE,
  METHOD_CONSUME,
  METHOD_GRPC,
} from './attrs';
import { entryOfKey, entryRoute, linkSpan, rawRoute, spanKey, unlinkSpan, type Entry } from './entry';
import { recordError } from './errors';
import { normalizeGenAiSpan } from './genai';
import { callerFromContext } from './propagation';
import { normalizeRoute, stripQueryString, stripQueryValues } from './route';
import { rateFromTraceState } from './sampler';

const URL_ATTRS = ['url.full', 'http.url', 'http.target'] as const;

function str(v: AttributeValue | undefined): string | undefined {
  return typeof v === 'string' && v !== '' ? v : undefined;
}

/** Query values never leave the process (AGENTS.md rule 6): keys are kept, values dropped. */
function scrubUrls(span: Span): void {
  const a = span.attributes;
  for (const k of URL_ATTRS) {
    const v = a[k];
    if (typeof v === 'string' && v.includes('?')) span.setAttribute(k, stripQueryValues(v));
  }
  const q = a['url.query'];
  if (typeof q === 'string' && q.includes('=')) span.setAttribute('url.query', stripQueryString(q));
}

/** Consumer process name: trooper.process → consumer group → destination (→ amqplib "<queue> process" span name). */
export function processOf(span: Span | ReadableSpan): string | undefined {
  const a = span.attributes;
  return (
    str(a[ATTR_PROCESS]) ??
    str(a['messaging.consumer.group.name']) ??
    str(a['messaging.kafka.consumer.group']) ??
    str(a['messaging.destination.name']) ??
    (span.name.endsWith(' process') && span.name.length > 8 ? span.name.slice(0, -8) : undefined) ??
    str(a['messaging.destination'])
  );
}

function grpcRoute(span: Span): string | undefined {
  const sys = str(span.attributes['rpc.system']);
  if (!sys || !sys.toLowerCase().startsWith('grpc')) return undefined;
  const svc = str(span.attributes['rpc.service']);
  const m = str(span.attributes['rpc.method']);
  return svc && m ? `/${svc}/${m}` : undefined;
}

function httpMethod(span: Span): string | undefined {
  const m = str(span.attributes['http.request.method']) ?? str(span.attributes['http.method']);
  if (!m) return undefined;
  return m === '_OTHER' ? m : m.toUpperCase();
}

/**
 * Adds the TaskTrooper conventions (.ai/contracts.md §4) to spans:
 * - entry (local-root SERVER / CONSUMER) spans: `trooper.sample_rate` (own rate for roots, upstream `tt=r:` rate for
 *   remote parents), `trooper.caller` from baggage `tt.svc`, `trooper.route` (server), `trooper.process` +
 *   `trooper.consumer.attempt` (consumer), `trooper.entry.route` / `trooper.entry.method`;
 * - every span below an entry: `trooper.entry.route` / `trooper.entry.method`;
 * - URL attributes: query values stripped;
 * - Vercel AI SDK provider-call spans: `gen_ai.operation.name` / `gen_ai.provider.name` filled in (no span is dropped).
 * Never throws.
 */
export class TrooperSpanProcessor implements SpanProcessor {
  readonly sampleRate: number;

  constructor(sampleRate = 1) {
    this.sampleRate = sampleRate > 0 && sampleRate <= 1 ? sampleRate : 1;
  }

  onStart(span: Span, parentContext: Context): void {
    try {
      scrubUrls(span);
      const parent = trace.getSpanContext(parentContext);
      const localRoot = !parent || !trace.isSpanContextValid(parent) || parent.isRemote === true;
      const key = spanKey(span.spanContext());
      if (localRoot) {
        if (span.kind !== SpanKind.SERVER && span.kind !== SpanKind.CONSUMER) return;
        this.startEntry(span, parentContext, parent?.isRemote ? parent.traceState : undefined, !!parent && trace.isSpanContextValid(parent), key);
        return;
      }
      const e = entryOfKey(spanKey(parent!));
      if (!e) return;
      linkSpan(key, e);
      if (!e.rpc) {
        const rpc = getRPCMetadata(parentContext);
        if (rpc) e.rpc = rpc;
      }
      setEntryAttrs(span, e, false);
    } catch (err) {
      recordError('span_processor', err);
    }
  }

  private startEntry(span: Span, parentContext: Context, remoteState: import('@opentelemetry/api').TraceState | undefined, hasParent: boolean, key: string): void {
    const consumer = span.kind === SpanKind.CONSUMER;
    const e: Entry = { root: span, kind: consumer ? 'consumer' : 'server' };
    span.setAttribute(ATTR_SAMPLE_RATE, hasParent ? (rateFromTraceState(remoteState) ?? this.sampleRate) : this.sampleRate);
    const caller = callerFromContext(parentContext);
    if (caller) span.setAttribute(ATTR_CALLER, caller);
    if (consumer) {
      e.method = METHOD_CONSUME;
      e.route = processOf(span);
      if (e.route) span.setAttribute(ATTR_PROCESS, e.route);
    } else {
      const grpc = grpcRoute(span);
      if (grpc) {
        e.route = grpc;
        e.method = METHOD_GRPC;
      } else {
        e.method = httpMethod(span);
      }
    }
    linkSpan(key, e);
  }

  onEnding(span: Span): void {
    try {
      scrubUrls(span);
      normalizeGenAiSpan(span);
      const e = entryOfKey(spanKey(span.spanContext()));
      if (!e) return;
      if (e.root === span) this.finishEntry(span, e);
      else setEntryAttrs(span, e, false);
    } catch (err) {
      recordError('span_processor', err);
    }
  }

  private finishEntry(span: Span, e: Entry): void {
    if (e.kind === 'consumer') {
      const p = str(span.attributes[ATTR_PROCESS]) ?? processOf(span);
      if (p) {
        e.route = p;
        span.setAttribute(ATTR_PROCESS, p);
      }
      if (e.attempt !== undefined && span.attributes[ATTR_CONSUMER_ATTEMPT] === undefined) {
        span.setAttribute(ATTR_CONSUMER_ATTEMPT, e.attempt);
      }
    } else {
      if (!e.method) e.method = httpMethod(span);
      const { route: raw, own } = rawRoute(e);
      const route = raw ? (e.method === METHOD_GRPC ? raw : normalizeRoute(raw)) : undefined;
      if (route) {
        span.setAttribute(ATTR_ROUTE, route);
        if (own && span.attributes['http.route'] === undefined && e.method !== METHOD_GRPC) span.setAttribute('http.route', raw!);
      }
    }
    setEntryAttrs(span, e, true);
  }

  onEnd(span: ReadableSpan): void {
    unlinkSpan(spanKey(span.spanContext()));
  }

  forceFlush(): Promise<void> {
    return Promise.resolve();
  }

  shutdown(): Promise<void> {
    return Promise.resolve();
  }
}

function setEntryAttrs(span: Span, e: Entry, overwrite: boolean): void {
  if (!overwrite && span.attributes[ATTR_ENTRY_ROUTE] !== undefined) return;
  const route = e.method === METHOD_GRPC ? e.route : entryRoute(e);
  if (!route) return;
  span.setAttribute(ATTR_ENTRY_ROUTE, route);
  if (e.method) span.setAttribute(ATTR_ENTRY_METHOD, e.method);
}
