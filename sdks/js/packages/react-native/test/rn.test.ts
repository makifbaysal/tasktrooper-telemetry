import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AppState, NativeModules, TurboModuleRegistry } from 'react-native';
import type { RumBatch, RumEvent } from '@trooper/core';
import { captureError, clearUser, close, flush, getSdk, identify, init, markAppReady, NATIVE_CHANNEL, NATIVE_MODULE_NAME, trooperNavigation } from '../src/index';
import { defaultNativeBridge } from '../src/bridge';

const TURBO = (TurboModuleRegistry as unknown as { modules: Record<string, unknown> }).modules;

const OPTIONS = { dsn: 'http://tt_dev_ingest_key@localhost:4318/1', app: 'courier-app', version: '2.1.0', trackNetwork: false };
const tick = (ms = 20) => new Promise((r) => setTimeout(r, ms));
const AS = AppState as typeof AppState & { __emit(s: string): void; __count(): number };

let batches: RumBatch[];
beforeEach(() => {
  batches = [];
  globalThis.fetch = (async (_u: unknown, init?: RequestInit) => {
    batches.push(JSON.parse(String(init?.body)));
    return new Response('{}');
  }) as typeof fetch;
});
afterEach(async () => {
  await close();
  delete NativeModules[NATIVE_MODULE_NAME];
  delete NativeModules[NATIVE_CHANNEL];
  delete TURBO[NATIVE_MODULE_NAME];
});
const events = async (): Promise<RumEvent[]> => {
  await tick();
  await flush();
  return batches.flatMap((b) => b.events);
};

interface EU {
  getGlobalHandler(): (e: unknown, fatal?: boolean) => void;
  setGlobalHandler(h: (e: unknown, fatal?: boolean) => void): void;
}
function installErrorUtils(prev: (e: unknown, fatal?: boolean) => void) {
  let h = prev;
  const eu: EU = { getGlobalHandler: () => h, setGlobalHandler: (n) => void (h = n) };
  (globalThis as { ErrorUtils?: EU }).ErrorUtils = eu;
  return eu;
}

describe('react-native', () => {
  it('resource: platform, os, device', async () => {
    init(OPTIONS);
    identify('courier-7');
    captureError(new Error('x'));
    await events();
    expect(batches[0]!.resource).toMatchObject({
      app: 'courier-app', version: '2.1.0', platform: 'PLATFORM_REACT_NATIVE', sdk: { name: '@trooper/react-native' },
      os: { name: 'iOS', version: '19.0' }, device: { type: 'mobile', screen_width: 1179, screen_height: 2556 },
    });
  });

  it('global error handler is chained and fatal errors are crashes', async () => {
    const prev = vi.fn();
    const eu = installErrorUtils(prev);
    init(OPTIONS);
    eu.getGlobalHandler()(new TypeError('boom'), true);
    eu.getGlobalHandler()(new Error('soft'), false);
    expect(prev).toHaveBeenCalledTimes(2);
    expect(prev.mock.calls[0]![1]).toBe(true);
    const errs = (await events()).filter((e) => e.type === 'ERROR');
    expect(errs.map((e) => e.error)).toEqual([
      expect.objectContaining({ type: 'TypeError', message: 'boom', source: 'crash', is_crash: true }),
      expect.objectContaining({ message: 'soft', source: 'source', is_crash: false }),
    ]);
    await close();
    expect(eu.getGlobalHandler()).toBe(prev); // restored
  });

  it('AppState background → VIEW_END + flush; active → screen resumes', async () => {
    init(OPTIONS);
    const ref = { name: 'Deliveries' };
    const nav = trooperNavigation({ getCurrentRoute: () => ref });
    nav.onReady();
    await tick();
    expect(batches).toHaveLength(0);
    AS.__emit('background');
    await tick();
    expect(batches.length).toBeGreaterThan(0); // flushed without waiting 5 s
    expect(batches.flatMap((b) => b.events).map((e) => e.type)).toContain('VIEW_END');
    AS.__emit('active');
    const starts = (await events()).filter((e) => e.type === 'VIEW_START');
    expect(starts.map((e) => e.view_name)).toEqual(['Deliveries', 'Deliveries']);
    expect(starts[0]!.view!.is_initial).toBe(true);
    expect(starts[1]!.view!.is_initial).toBe(false);
  });

  it('react-navigation: screen names, view timing, APP_START from js_init', async () => {
    init(OPTIONS);
    let route = { name: 'Deliveries' };
    const nav = trooperNavigation({ getCurrentRoute: () => route });
    nav.onReady();
    await tick();
    route = { name: 'DeliveryDetail' };
    nav.onStateChange();
    nav.onStateChange(); // same screen: ignored
    const evs = await events();
    expect(evs.filter((e) => e.type === 'VIEW_START').map((e) => e.view_name)).toEqual(['Deliveries', 'DeliveryDetail']);
    const end = evs.find((e) => e.type === 'VIEW_END')!;
    expect(end.view_name).toBe('Deliveries');
    expect(end.view!.time_spent_ms).toBeGreaterThanOrEqual(10);
    const start = evs.find((e) => e.type === 'VIEW_START')!;
    expect(start.view!.loading_time_ms).toBeGreaterThanOrEqual(0);
    const app = evs.find((e) => e.type === 'APP_START')!;
    expect(app.app_start!.kind).toBe('cold');
    expect(app.app_start!.duration_ms).toBeGreaterThanOrEqual(0);
    expect(app.attributes).toEqual({ start_reference: 'js_init' });
    markAppReady(); // only once
    expect((await events()).filter((e) => e.type === 'APP_START')).toHaveLength(1);
  });

  it('native bridge: adopts native session, pushes context, uses process start time', async () => {
    const setContext = vi.fn();
    NativeModules[NATIVE_MODULE_NAME] = {
      getContext: async () => ({ session_id: 'native-sess', anonymous_id: 'native-anon' }),
      setContext,
      getProcessStartTime: async () => Date.now() - 1500,
    };
    init(OPTIONS);
    captureError(new Error('early'));
    await tick();
    identify('u9');
    markAppReady();
    const evs = await events();
    expect(new Set(evs.map((e) => e.session_id))).toEqual(new Set(['native-sess']));
    expect(new Set(evs.map((e) => e.anonymous_id))).toEqual(new Set(['native-anon']));
    expect(setContext).toHaveBeenLastCalledWith(expect.objectContaining({ session_id: 'native-sess', anonymous_id: 'native-anon', user_id: 'u9' }));
    const app = evs.find((e) => e.type === 'APP_START')!;
    expect(app.attributes).toEqual({ start_reference: 'process' });
    expect(app.app_start!.duration_ms).toBeGreaterThanOrEqual(1450);
  });

  it('setContext carries the JS sampling decision (hash of the session id), also after rotation', async () => {
    const { hashToUnit } = await import('@trooper/core');
    const setContext = vi.fn();
    NativeModules[NATIVE_MODULE_NAME] = { getContext: async () => null, setContext };
    init({ ...OPTIONS, sampleRate: 0.5 });
    captureError(new Error('x'));
    await tick();
    const first = setContext.mock.calls.at(-1)![0];
    expect(first.session_id).toEqual(expect.any(String));
    expect(first.sampled).toBe(hashToUnit(first.session_id) < 0.5);
    clearUser(); // new session
    captureError(new Error('y'));
    await tick();
    const second = setContext.mock.calls.at(-1)![0];
    expect(second.session_id).not.toBe(first.session_id);
    expect(second.sampled).toBe(hashToUnit(second.session_id) < 0.5);
    await close();
    setContext.mockClear();
    init(OPTIONS); // sampleRate 1 → always sampled
    captureError(new Error('z'));
    await tick();
    expect(setContext.mock.calls.at(-1)![0].sampled).toBe(true);
  });

  it('native module lookup: TurboModule, then NativeModules.TrooperRum, then the legacy channel name', () => {
    expect(NATIVE_MODULE_NAME).toBe('TrooperRum');
    expect(defaultNativeBridge()).toBeUndefined();
    const legacy = { getContext: async () => null };
    NativeModules[NATIVE_CHANNEL] = legacy;
    expect(defaultNativeBridge()).toBe(legacy);
    const bridged = { getContext: async () => null };
    NativeModules[NATIVE_MODULE_NAME] = bridged;
    expect(defaultNativeBridge()).toBe(bridged);
    const turbo = { getContext: async () => null };
    TURBO[NATIVE_MODULE_NAME] = turbo;
    expect(defaultNativeBridge()).toBe(turbo);
  });

  it('uses the TurboModule for process start time', async () => {
    TURBO[NATIVE_MODULE_NAME] = { getContext: async () => null, setContext: () => undefined, getProcessStartTime: async () => Date.now() - 900 };
    init(OPTIONS);
    markAppReady();
    const app = (await events()).find((e) => e.type === 'APP_START')!;
    expect(app.attributes).toEqual({ start_reference: 'process' });
    expect(app.app_start!.duration_ms).toBeGreaterThanOrEqual(850);
  });

  it('missing / throwing native module is silent', async () => {
    NativeModules[NATIVE_MODULE_NAME] = {
      getContext: () => {
        throw new Error('not linked');
      },
      setContext: () => Promise.reject(new Error('x')),
    };
    expect(() => init(OPTIONS)).not.toThrow();
    markAppReady();
    expect((await events()).some((e) => e.type === 'APP_START')).toBe(true);
    expect(getSdk()).toBeDefined();
  });

  it('persists the anonymous id via AsyncStorage-like storage and queues offline batches there', async () => {
    const mem = new Map<string, string>([['trooper:anon', 'stored-anon']]);
    const storage = { getItem: async (k: string) => mem.get(k) ?? null, setItem: async (k: string, v: string) => void mem.set(k, v) };
    globalThis.fetch = (async () => new Response('', { status: 503 })) as unknown as typeof fetch;
    init({ ...OPTIONS, storage });
    captureError(new Error('offline'));
    await tick();
    await flush();
    await tick();
    const queued = JSON.parse(mem.get('trooper:offline') ?? '[]') as string[];
    expect(queued).toHaveLength(1);
    expect(JSON.parse(queued[0]!).events[0].anonymous_id).toBe('stored-anon');
  });
});
