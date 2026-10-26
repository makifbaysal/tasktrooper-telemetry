import { vi } from 'vitest';
import type { RumBatch, RumEvent } from '@trooper/core';
import { close, flush, init, type BrowserOptions } from '../src/index';

export const DSN = 'http://tt_dev_ingest_key@localhost:4318/1';

/** Init the SDK with a fetch stub capturing every batch sent to /v1/rum. */
export function setup(opts: Partial<BrowserOptions> = {}) {
  const batches: RumBatch[] = [];
  const apiCalls: { url: string; init?: RequestInit }[] = [];
  const fetchStub = vi.fn(async (input: unknown, init?: RequestInit) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes('/v1/rum')) batches.push(JSON.parse(String(init?.body)));
    else apiCalls.push({ url, init });
    return new Response('{}', { status: 200, headers: { 'content-length': '2' } });
  });
  globalThis.fetch = fetchStub as unknown as typeof fetch;
  const sdk = init({ dsn: DSN, app: 'shop-web', version: '1.0.0', environment: 'test', persist: false, trackVitals: false, ...opts })!;
  const events = async (): Promise<RumEvent[]> => {
    await new Promise((r) => setTimeout(r, 300)); // let the current view settle (VIEW_START carries loading time)
    await flush();
    return batches.flatMap((b) => b.events);
  };
  return { sdk, batches, apiCalls, fetchStub, events };
}

export async function teardown() {
  await close();
  history.replaceState(null, '', '/');
}
