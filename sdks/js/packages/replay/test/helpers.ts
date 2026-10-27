import { vi } from 'vitest';
import { close, init, type BrowserOptions } from '@trooper/browser';
import { replayIntegration, type ReplayOptions, type ReplaySegmentPayload } from '../src/index';

import { rr } from './rrweb-mock';

export { CLICK, emit, FULL, INPUT, META, MUTATION, rr } from './rrweb-mock';

export const DSN = 'http://tt_dev_ingest_key@localhost:4318/1';

export interface Sent {
  url: string;
  init: RequestInit;
  payload: ReplaySegmentPayload;
}

/** Init `@trooper/browser` + replay with a fetch stub; replay requests are decoded into `sent`. */
export function setup(replay: ReplayOptions = {}, opts: Partial<BrowserOptions> = {}, respond: () => Response | Promise<Response> = ok) {
  const sent: Sent[] = [];
  const rum: unknown[] = [];
  const pending: Promise<unknown>[] = [];
  const fetchStub = vi.fn(async (input: unknown, reqInit?: RequestInit) => {
    const url = String(input);
    if (url.includes('/v1/replay')) {
      const p = decode(reqInit!).then((payload) => sent.push({ url, init: reqInit!, payload }));
      pending.push(p);
      await p;
    } else rum.push(reqInit?.body);
    return respond();
  });
  globalThis.fetch = fetchStub as unknown as typeof fetch;
  const integration = replayIntegration(replay);
  const sdk = init({ dsn: DSN, app: 'shop-web', version: '1.4.0', environment: 'production', persist: false, trackVitals: false, integrations: [integration], ...opts })!;
  const settle = async () => {
    for (let i = 0; i < 5; i++) {
      await Promise.all(pending.splice(0));
      await new Promise((r) => setTimeout(r, 0));
    }
  };
  return { sdk, integration, sent, rum, fetchStub, settle };
}

export const ok = () => new Response('', { status: 200 });

async function decode(init: RequestInit): Promise<ReplaySegmentPayload> {
  const h = (init.headers ?? {}) as Record<string, string>;
  let text: string;
  if (h['content-encoding'] === 'gzip') {
    const { gunzipSync } = await import('node:zlib');
    text = gunzipSync(Buffer.from(init.body as ArrayBuffer)).toString('utf8');
  } else text = String(init.body);
  return JSON.parse(text);
}

export async function teardown() {
  await close();
  rr.opts = undefined;
  rr.calls = rr.stops = rr.snapshots = 0;
  rr.throwOnRecord = false;
  try {
    sessionStorage.clear();
    localStorage.clear();
  } catch {
    /* ignore */
  }
  vi.restoreAllMocks();
  vi.useRealTimers();
}
