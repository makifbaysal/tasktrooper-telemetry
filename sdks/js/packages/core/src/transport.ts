import { reportInternal } from './util';

export type SendResult =
  | { status: 'ok' }
  /** 429 / 5xx / network failure: keep the batch and retry later. */
  | { status: 'retry'; retryAfterMs?: number }
  /** Other 4xx: the batch will never be accepted, drop it. */
  | { status: 'drop'; code?: number };

export interface SendOptions {
  /** Page is being hidden/unloaded: use a fire-and-forget channel (sendBeacon / keepalive). */
  beacon?: boolean;
}

export interface Transport {
  send(body: string, opts?: SendOptions): Promise<SendResult>;
}

/** Map an HTTP response to a retry decision (contracts.md §2). */
export function classifyStatus(status: number, retryAfter?: string | null): SendResult {
  if (status >= 200 && status < 300) return { status: 'ok' };
  if (status === 429 || status >= 500 || status === 0) {
    const ra = parseRetryAfter(retryAfter);
    return ra === undefined ? { status: 'retry' } : { status: 'retry', retryAfterMs: ra };
  }
  return { status: 'drop', code: status };
}

export function parseRetryAfter(v: string | null | undefined, nowMs = Date.now()): number | undefined {
  if (!v) return undefined;
  const secs = Number(v);
  if (!Number.isNaN(secs)) return Math.max(0, secs * 1000);
  const date = Date.parse(v);
  return Number.isNaN(date) ? undefined : Math.max(0, date - nowMs);
}

export const BACKOFF_MIN_MS = 1000;
export const BACKOFF_MAX_MS = 60000;

/** Exponential backoff 1 s → 60 s with jitter (50–100 % of the step). `attempt` starts at 0. */
export function backoffDelay(attempt: number, random: () => number = Math.random): number {
  const base = Math.min(BACKOFF_MAX_MS, BACKOFF_MIN_MS * 2 ** Math.max(0, attempt));
  return Math.round(base * (0.5 + random() * 0.5));
}

type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export interface FetchTransportOptions {
  endpoint: string;
  key: string;
  /** Unpatched fetch; defaults to globalThis.fetch captured at construction time. */
  fetch?: FetchLike;
}

/** Keepalive requests are limited to 64 KiB of in-flight body by browsers. */
export const KEEPALIVE_LIMIT = 60000;

/** `POST /v1/rum` with `x-trooper-key`. Works in browsers, React Native and Node 18+. */
export class FetchTransport implements Transport {
  private readonly fetchImpl: FetchLike | undefined;

  constructor(private readonly opts: FetchTransportOptions) {
    const f = opts.fetch ?? (globalThis as { fetch?: FetchLike }).fetch;
    this.fetchImpl = f ? f.bind(globalThis) : undefined;
  }

  async send(body: string, opts?: SendOptions): Promise<SendResult> {
    if (!this.fetchImpl) return { status: 'drop' };
    try {
      const res = await this.fetchImpl(this.opts.endpoint, {
        method: 'POST',
        body,
        headers: { 'content-type': 'application/json', 'x-trooper-key': this.opts.key },
        keepalive: !!opts?.beacon && body.length < KEEPALIVE_LIMIT,
        credentials: 'omit',
      } as RequestInit);
      return classifyStatus(res.status, res.headers?.get?.('retry-after'));
    } catch (e) {
      reportInternal('transport.fetch', e);
      return { status: 'retry' };
    }
  }
}
