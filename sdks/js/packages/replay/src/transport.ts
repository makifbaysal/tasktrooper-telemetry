import type { IntegrationClient } from '@trooper/browser';

/** Browsers cap the total in-flight body of keepalive requests at 64 KiB. */
export const KEEPALIVE_LIMIT = 60000;
/** Segments waiting for delivery (retry/backoff) are capped; the oldest are dropped first. */
export const MAX_QUEUE_BYTES = 5 * 1024 * 1024;
export const MAX_QUEUE = 30;

export interface ReplayStats {
  sent: number;
  dropped: number;
  retried: number;
}

const canGzip = (): boolean => typeof CompressionStream === 'function' && typeof Blob === 'function' && typeof Response === 'function';

async function gzip(body: string): Promise<ArrayBuffer> {
  const stream = new Blob([body]).stream().pipeThrough(new CompressionStream('gzip'));
  return new Response(stream).arrayBuffer();
}

/**
 * `POST <origin>/v1/replay?key=<key>` (contracts.md §23, §2). The key travels in the query so plain requests stay
 * CORS-simple (`text/plain` JSON, no preflight); gzip bodies add `Content-Encoding: gzip` (allowed by the preflight).
 * 429/5xx/network → exponential backoff (1 s → 60 s), other 4xx → drop. Never throws.
 */
export class ReplayTransport {
  readonly stats: ReplayStats = { sent: 0, dropped: 0, retried: 0 };
  private readonly url: string;
  private queue: string[] = [];
  private queued = 0;
  private attempt = 0;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private running: Promise<void> | undefined;
  private closed = false;

  constructor(private readonly ic: IntegrationClient) {
    this.url = `${ic.config.endpoint}/v1/replay?key=${encodeURIComponent(ic.config.key)}`;
  }

  /** Queue a segment body (JSON) for delivery. */
  enqueue(body: string): Promise<void> {
    if (this.closed) return Promise.resolve();
    this.queue.push(body);
    this.queued += body.length;
    while (this.queue.length > 1 && (this.queue.length > MAX_QUEUE || this.queued > MAX_QUEUE_BYTES)) {
      this.queued -= this.queue.shift()!.length;
      this.stats.dropped++;
    }
    return this.pump();
  }

  /** Resolves when the queue is empty or waiting for a retry. */
  pump(): Promise<void> {
    if (this.timer || this.closed) return Promise.resolve();
    if (!this.running) {
      this.running = this.loop().finally(() => {
        this.running = undefined;
      });
    }
    return this.running;
  }

  private async loop(): Promise<void> {
    while (this.queue.length && !this.timer) {
      const body = this.queue[0]!;
      let r: { status: string; retryAfterMs?: number };
      try {
        r = await this.send(body);
      } catch (e) {
        this.ic.reportInternal('replay.send', e);
        r = { status: 'retry' };
      }
      if (r.status === 'retry') {
        this.stats.retried++;
        const delay = Math.max(r.retryAfterMs ?? 0, this.ic.backoffDelay(this.attempt++));
        this.timer = setTimeout(() => {
          this.timer = undefined;
          void this.pump();
        }, delay);
        return;
      }
      this.attempt = 0;
      if (this.queue[0] === body) {
        this.queue.shift();
        this.queued -= body.length;
      }
      if (r.status === 'ok') this.stats.sent++;
      else this.stats.dropped++;
    }
  }

  private async send(body: string): Promise<{ status: string; retryAfterMs?: number }> {
    const f = this.ic.fetch;
    if (!f) return { status: 'drop' };
    let init: RequestInit = plain(body, false);
    if (canGzip()) {
      try {
        init = {
          method: 'POST',
          body: await gzip(body),
          headers: { 'content-type': 'application/json', 'content-encoding': 'gzip' },
          credentials: 'omit',
        };
      } catch (e) {
        this.ic.reportInternal('replay.gzip', e);
      }
    }
    try {
      const res = await f(this.url, init);
      return this.ic.classifyStatus(res.status, res.headers?.get?.('retry-after'));
    } catch (e) {
      this.ic.reportInternal('replay.fetch', e);
      return { status: 'retry' };
    }
  }

  /**
   * Page is being hidden/unloaded: fire the request synchronously with `keepalive` (plain JSON — compression is async
   * and the page may be frozen before it finishes). Bodies over the keepalive limit go out as a regular fetch.
   */
  sendNow(body: string): void {
    const f = this.ic.fetch;
    if (!f || this.closed) return;
    try {
      f(this.url, plain(body, body.length <= KEEPALIVE_LIMIT)).then(
        (res) => (res.status >= 200 && res.status < 300 ? this.stats.sent++ : this.stats.dropped++),
        () => this.stats.dropped++,
      );
    } catch (e) {
      this.ic.reportInternal('replay.keepalive', e);
    }
  }

  close(): void {
    this.closed = true;
    if (this.timer) clearTimeout(this.timer);
    this.timer = undefined;
  }
}

const plain = (body: string, keepalive: boolean): RequestInit => ({
  method: 'POST',
  body,
  // CORS-safelisted content type: no preflight (the gateway parses it as JSON, contracts.md §2).
  headers: { 'content-type': 'text/plain;charset=UTF-8' },
  keepalive,
  credentials: 'omit',
});
