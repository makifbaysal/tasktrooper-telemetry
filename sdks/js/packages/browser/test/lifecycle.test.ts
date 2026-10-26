import 'fake-indexeddb/auto';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { IdbOfflineStore } from '../src/idb';
import { detectBrowser, detectDeviceType, detectOs } from '../src/env';
import { track } from '../src/index';
import { DSN, setup, teardown } from './helpers';

afterEach(teardown);

describe('page hide', () => {
  it('flushes via sendBeacon with ?key= on visibilitychange hidden, VIEW_END on pagehide', async () => {
    const sent: { url: string; body: string }[] = [];
    const beacon = vi.fn((url: string, data: Blob) => {
      void data.text().then((body) => sent.push({ url, body }));
      return true;
    });
    Object.defineProperty(navigator, 'sendBeacon', { value: beacon, configurable: true });
    const { fetchStub } = setup();
    track('a');
    Object.defineProperty(document, 'visibilityState', { value: 'hidden', configurable: true });
    document.dispatchEvent(new Event('visibilitychange', { bubbles: true }));
    window.dispatchEvent(new Event('visibilitychange'));
    expect(beacon).toHaveBeenCalledTimes(1);
    expect(beacon.mock.calls[0]![0]).toBe('http://localhost:4318/v1/rum?key=tt_dev_ingest_key');
    expect((beacon.mock.calls[0]![1] as Blob).type).toContain('text/plain');
    window.dispatchEvent(new Event('pagehide'));
    await new Promise((r) => setTimeout(r, 10));
    const types = sent.flatMap((s) => JSON.parse(s.body).events.map((e: { type: string }) => e.type));
    // VIEW_START is emitted on hide (load not observed yet) with its original navigation timestamp.
    expect(types).toEqual(['SESSION_START', 'CUSTOM', 'VIEW_START', 'VIEW_END']);
    expect(fetchStub).not.toHaveBeenCalled();
    Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true });
    Reflect.deleteProperty(navigator, 'sendBeacon');
  });

  it('resource carries web platform, sdk and device info', async () => {
    const { batches, events } = setup();
    track('x');
    await events();
    expect(batches[0]!.resource).toMatchObject({
      app: 'shop-web', version: '1.0.0', environment: 'test', platform: 'PLATFORM_WEB', sdk: { name: '@trooper/browser' },
    });
    expect(DSN).toContain('localhost');
  });
});

describe('env detection', () => {
  it.each([
    ['Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36', 'Chrome', 'macOS', 'desktop'],
    ['Mozilla/5.0 (iPhone; CPU iPhone OS 19_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/19.0 Mobile/15E148 Safari/604.1', 'Safari', 'iOS', 'mobile'],
    ['Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0', 'Edge', 'Windows', 'desktop'],
    ['Mozilla/5.0 (Linux; Android 16; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0 Mobile Safari/537.36', 'Chrome', 'Android', 'mobile'],
  ])('%#', (ua, browser, os, type) => {
    expect(detectBrowser(ua).name).toBe(browser);
    expect(detectOs(ua).name).toBe(os);
    expect(detectDeviceType(ua)).toBe(type);
  });
});

describe('IndexedDB offline store', () => {
  it('FIFO with byte cap, persists across instances', async () => {
    const a = new IdbOfflineStore('t1', 10);
    await a.push('aaaa');
    await a.push('bbbb');
    await a.push('cccc');
    expect(await a.size()).toBe(2);
    const b = new IdbOfflineStore('t1', 10);
    expect(await b.peek()).toBe('bbbb');
    await b.shift();
    expect(await b.peek()).toBe('cccc');
    expect(await b.size()).toBe(1);
  });
});
