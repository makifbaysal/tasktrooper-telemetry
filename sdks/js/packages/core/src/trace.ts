import { randomHex } from './ids';

export interface TraceContext {
  traceId: string;
  spanId: string;
  /** W3C `traceparent` header value: `00-<32 hex>-<16 hex>-01`. */
  traceparent: string;
}

export function createTraceContext(sampled = true): TraceContext {
  const traceId = randomHex(16);
  const spanId = randomHex(8);
  return { traceId, spanId, traceparent: `00-${traceId}-${spanId}-${sampled ? '01' : '00'}` };
}

export type PropagationTarget = string | RegExp;

const originOf = (url: string): string | undefined => {
  const m = /^([a-z][a-z0-9+.-]*:\/\/[^/?#]+)/i.exec(url);
  return m ? m[1]!.toLowerCase() : undefined;
};

/** Resolve a possibly relative URL against `base` (string ops; works without a URL implementation). */
export function absoluteUrl(url: string, base?: string): string {
  if (/^[a-z][a-z0-9+.-]*:/i.test(url) || !base) return url;
  const origin = originOf(base) ?? '';
  if (url.startsWith('//')) return (/^([a-z]+:)/i.exec(base)?.[1] ?? 'https:') + url;
  if (url.startsWith('/')) return origin + url;
  const dir = base.split(/[?#]/)[0]!.replace(/[^/]*$/, '');
  return dir + url;
}

/**
 * Should a `traceparent` header be injected for `url`?
 * - RegExp targets are tested against the absolute URL.
 * - String targets match by prefix (`https://api.shop.com`) or exact origin.
 * - With no targets configured, only same-origin requests (relative to `currentOrigin`) match — safe w.r.t. CORS.
 */
export function shouldPropagate(url: string, targets: PropagationTarget[] | undefined, currentOrigin?: string): boolean {
  const abs = absoluteUrl(url, currentOrigin);
  if (!targets) return !!currentOrigin && originOf(abs) === originOf(currentOrigin);
  for (const t of targets) {
    if (typeof t === 'string') {
      if (t && (abs.startsWith(t) || originOf(abs) === originOf(t))) return true;
    } else if (t instanceof RegExp) {
      t.lastIndex = 0;
      if (t.test(abs)) return true;
    }
  }
  return false;
}

/** W3C baggage key carrying the calling service name (read by backend SDKs into `trooper.caller`). */
export const BAGGAGE_SERVICE_KEY = 'tt.svc';
/** W3C baggage limit for the whole header. */
export const BAGGAGE_MAX_BYTES = 8192;

const utf8Len = (s: string): number => {
  let n = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c < 0x80) n += 1;
    else if (c < 0x800) n += 2;
    else if (c >= 0xd800 && c <= 0xdbff) {
      n += 4;
      i++;
    } else n += 3;
  }
  return n;
};

/**
 * Merge `tt.svc=<service>` into an existing W3C `baggage` header value.
 * Any existing `tt.svc` member is replaced; other members are kept as-is.
 * Returns the existing value unchanged when the service name is empty, the result would exceed
 * {@link BAGGAGE_MAX_BYTES}, or anything fails (never throws).
 */
export function mergeBaggage(existing: string | null | undefined, service: string | undefined): string | undefined {
  const prev = existing ?? undefined;
  try {
    if (!service) return prev;
    const own = `${BAGGAGE_SERVICE_KEY}=${encodeURIComponent(service)}`;
    const members = (prev ?? '')
      .split(',')
      .map((m) => m.trim())
      .filter((m) => m && m.split(/[=;]/)[0]!.trim() !== BAGGAGE_SERVICE_KEY);
    const out = [own, ...members].join(',');
    return utf8Len(out) <= BAGGAGE_MAX_BYTES ? out : prev;
  } catch {
    return prev;
  }
}
