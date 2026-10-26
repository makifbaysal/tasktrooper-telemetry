import type { EventInput } from './client';
import { absoluteUrl, createTraceContext, mergeBaggage, shouldPropagate, type PropagationTarget, type TraceContext } from './trace';
import { byteLength, reportInternal } from './util';

export interface NetworkInstrumentationOptions {
  capture(e: EventInput): unknown;
  tracePropagationTargets?: PropagationTarget[];
  /**
   * Client app (service) name sent as W3C `baggage: tt.svc=<name>` next to `traceparent`
   * (same targets), so backends can attribute the call (`trooper.caller`).
   */
  serviceName?: string;
  /** Origin of the current page (web) used to resolve relative URLs and for same-origin propagation. */
  currentOrigin?: () => string | undefined;
  /** URL prefixes never instrumented (the ingest endpoint itself). */
  ignoreUrls?: string[];
  /** Activity hooks (dead-click detection, SPA settle). */
  onStart?: () => void;
  onEnd?: () => void;
}

const perfNow = (): number => {
  const p = (globalThis as { performance?: { now?: () => number } }).performance;
  return p && typeof p.now === 'function' ? p.now() : Date.now();
};

export function bodySize(body: unknown): number | undefined {
  try {
    if (body == null) return undefined;
    if (typeof body === 'string') return byteLength(body);
    const b = body as { size?: number; byteLength?: number };
    if (typeof b.byteLength === 'number') return b.byteLength;
    if (typeof b.size === 'number' && typeof Blob !== 'undefined' && body instanceof Blob) return b.size;
    if (typeof URLSearchParams !== 'undefined' && body instanceof URLSearchParams) return byteLength(body.toString());
  } catch {
    /* ignore */
  }
  return undefined;
}

const toNum = (v: string | null | undefined): number | undefined => {
  const n = v == null ? NaN : parseInt(v, 10);
  return Number.isFinite(n) && n >= 0 ? n : undefined;
};

class Tracker {
  constructor(private readonly o: NetworkInstrumentationOptions) {}

  resolve(url: string): string {
    return absoluteUrl(url, this.o.currentOrigin?.());
  }

  ignored(abs: string): boolean {
    return !!this.o.ignoreUrls?.some((p) => p && abs.startsWith(p));
  }

  trace(abs: string): TraceContext | undefined {
    return shouldPropagate(abs, this.o.tracePropagationTargets, this.o.currentOrigin?.()) ? createTraceContext() : undefined;
  }

  start(): number {
    try {
      this.o.onStart?.();
    } catch (e) {
      reportInternal('network.onStart', e);
    }
    return perfNow();
  }

  end(
    initiator: 'fetch' | 'xhr',
    url: string,
    method: string,
    t0: number,
    status: number,
    tc: TraceContext | undefined,
    reqBytes: number | undefined,
    resBytes: number | undefined,
  ): void {
    try {
      this.o.onEnd?.();
      this.o.capture({
        type: 'RESOURCE',
        resource: {
          url,
          method,
          status_code: status,
          duration_ms: Math.max(0, Math.round((perfNow() - t0) * 100) / 100),
          request_bytes: reqBytes,
          response_bytes: resBytes,
          trace_id: tc?.traceId,
          span_id: tc?.spanId,
          initiator,
        },
      });
    } catch (e) {
      reportInternal('network.end', e);
    }
  }
}

type AnyFetch = (input: unknown, init?: RequestInit) => Promise<Response>;

/** Patch `globalThis.fetch`. Returns a function restoring the original. */
export function instrumentFetch(opts: NetworkInstrumentationOptions): () => void {
  const g = globalThis as { fetch?: AnyFetch };
  const orig = g.fetch;
  if (typeof orig !== 'function') return () => undefined;
  const tr = new Tracker(opts);

  const patched: AnyFetch = function (this: unknown, input: unknown, init?: RequestInit) {
    let ctx: { url: string; method: string; t0: number; tc?: TraceContext; req?: number } | undefined;
    try {
      const isReq = typeof Request !== 'undefined' && input instanceof Request;
      const raw =
        typeof input === 'string' ? input : isReq ? (input as Request).url : String((input as { href?: string })?.href ?? input);
      const url = tr.resolve(raw);
      if (!tr.ignored(url)) {
        const method = String(init?.method || (isReq ? (input as Request).method : '') || 'GET').toUpperCase();
        const tc = typeof Headers !== 'undefined' ? tr.trace(url) : undefined;
        let finalTc: TraceContext | undefined;
        if (tc) {
          const headers = new Headers(init?.headers ?? (isReq ? (input as Request).headers : undefined));
          if (!headers.has('traceparent')) {
            headers.set('traceparent', tc.traceparent);
            finalTc = tc;
          }
          const bg = mergeBaggage(headers.get('baggage'), opts.serviceName);
          if (bg) headers.set('baggage', bg);
          init = { ...init, headers };
        }
        ctx = { url, method, t0: tr.start(), tc: finalTc, req: bodySize(init?.body) };
      }
    } catch (e) {
      reportInternal('fetch.before', e);
      ctx = undefined;
    }
    const p = orig.call(this, input, init);
    if (ctx) {
      const c = ctx;
      p.then(
        (res) => tr.end('fetch', c.url, c.method, c.t0, res.status, c.tc, c.req, toNum(res.headers?.get('content-length'))),
        () => tr.end('fetch', c.url, c.method, c.t0, 0, c.tc, c.req, undefined),
      );
    }
    return p;
  };
  g.fetch = patched;
  return () => {
    if (g.fetch === patched) g.fetch = orig;
  };
}

interface XhrState {
  method: string;
  url: string;
  hasTraceparent?: boolean;
  /** `baggage` set by the app; held back and merged with `tt.svc` at send time. */
  baggage?: string;
}

const XHR_STATE = '__trooper_xhr';

/** Patch `XMLHttpRequest.prototype`. Returns a function restoring the originals. */
export function instrumentXhr(opts: NetworkInstrumentationOptions): () => void {
  const X = (globalThis as { XMLHttpRequest?: typeof XMLHttpRequest }).XMLHttpRequest;
  if (!X || !X.prototype) return () => undefined;
  const proto = X.prototype;
  const { open, send, setRequestHeader } = proto;
  const tr = new Tracker(opts);
  type Xhr = XMLHttpRequest & { [XHR_STATE]?: XhrState };

  proto.open = function (this: Xhr, method: string, url: string | URL, ...rest: unknown[]) {
    try {
      this[XHR_STATE] = { method: String(method || 'GET').toUpperCase(), url: tr.resolve(String(url)) };
    } catch (e) {
      reportInternal('xhr.open', e);
    }
    return (open as (...a: unknown[]) => void).call(this, method, url, ...rest);
  } as typeof proto.open;

  proto.setRequestHeader = function (this: Xhr, name: string, value: string) {
    const st = this[XHR_STATE];
    if (st) {
      const n = String(name).toLowerCase();
      if (n === 'traceparent') st.hasTraceparent = true;
      else if (n === 'baggage' && opts.serviceName) {
        // XHR would join repeated headers; defer so send() can merge into a single value.
        st.baggage = st.baggage ? `${st.baggage},${value}` : String(value);
        return;
      }
    }
    return setRequestHeader.call(this, name, value);
  };

  proto.send = function (this: Xhr, body?: Document | XMLHttpRequestBodyInit | null) {
    const st = this[XHR_STATE];
    let baggage = st?.baggage;
    try {
      if (st && !tr.ignored(st.url)) {
        let tc: TraceContext | undefined;
        const propagate = tr.trace(st.url);
        if (propagate) baggage = mergeBaggage(baggage, opts.serviceName);
        if (!st.hasTraceparent && propagate) {
          tc = propagate;
          setRequestHeader.call(this, 'traceparent', tc.traceparent);
        }
        const t0 = tr.start();
        const req = bodySize(body);
        const xhr = this;
        const done = () => {
          xhr.removeEventListener('loadend', done);
          let res: number | undefined;
          try {
            res = toNum(xhr.getResponseHeader('content-length'));
            if (res === undefined && (xhr.responseType === '' || xhr.responseType === 'text') && typeof xhr.responseText === 'string')
              res = byteLength(xhr.responseText);
          } catch {
            /* ignore */
          }
          tr.end('xhr', st.url, st.method, t0, xhr.status || 0, tc, req, res);
        };
        this.addEventListener('loadend', done);
      }
    } catch (e) {
      reportInternal('xhr.send', e);
    }
    try {
      if (baggage) setRequestHeader.call(this, 'baggage', baggage);
    } catch (e) {
      reportInternal('xhr.baggage', e);
    }
    return send.call(this, body);
  };

  return () => {
    proto.open = open;
    proto.send = send;
    proto.setRequestHeader = setRequestHeader;
  };
}
