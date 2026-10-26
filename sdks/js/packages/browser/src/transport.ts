import { FetchTransport, KEEPALIVE_LIMIT, reportInternal, type SendOptions, type SendResult } from '@trooper/core';

/**
 * Normal batches: `fetch` with `x-trooper-key`.
 * Page hide: `navigator.sendBeacon(<endpoint>?key=<key>)` with a `text/plain` body (CORS-safelisted, no preflight;
 * beacons cannot carry headers — contracts.md §2), falling back to `fetch(keepalive)`.
 */
export class BrowserTransport extends FetchTransport {
  constructor(
    private readonly endpoint: string,
    private readonly key: string,
    fetchImpl?: (input: string, init?: RequestInit) => Promise<Response>,
  ) {
    super({ endpoint, key, fetch: fetchImpl });
  }

  override send(body: string, opts?: SendOptions): Promise<SendResult> {
    if (opts?.beacon && body.length < KEEPALIVE_LIMIT && typeof navigator !== 'undefined' && typeof navigator.sendBeacon === 'function') {
      try {
        const url = `${this.endpoint}?key=${encodeURIComponent(this.key)}`;
        if (navigator.sendBeacon(url, new Blob([body], { type: 'text/plain;charset=UTF-8' }))) return Promise.resolve({ status: 'ok' });
      } catch (e) {
        reportInternal('sendBeacon', e);
      }
    }
    return super.send(body, opts);
  }
}
