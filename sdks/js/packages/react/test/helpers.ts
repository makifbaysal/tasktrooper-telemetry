import { close, flush, type BrowserOptions } from '@trooper/browser';
import type { RumBatch, RumEvent } from '@trooper/core';

export const OPTIONS: BrowserOptions = {
  dsn: 'http://tt_dev_ingest_key@localhost:4318/1', app: 'shop-web', persist: false, trackVitals: false, trackActions: false,
};

export function stubIngest() {
  const batches: RumBatch[] = [];
  globalThis.fetch = (async (_u: unknown, init?: RequestInit) => {
    batches.push(JSON.parse(String(init?.body)));
    return new Response('{}');
  }) as typeof fetch;
  return async (): Promise<RumEvent[]> => {
    await new Promise((r) => setTimeout(r, 300)); // let the current view settle (VIEW_START carries loading time)
    await flush();
    return batches.flatMap((b) => b.events);
  };
}

export const viewStarts = (evs: RumEvent[]) => evs.filter((e) => e.type === 'VIEW_START').map((e) => e.view_name);

export async function reset() {
  await close();
  history.replaceState(null, '', '/');
}
