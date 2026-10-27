import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// Fresh module state per test (the one-time warning and the started flag are module-level).
async function load() {
  vi.resetModules();
  const rn = await import('react-native');
  const sdk = await import('../src/index');
  return { rn, sdk };
}

const OPTIONS = { dsn: 'http://tt_dev_ingest_key@localhost:4318/1', app: 'courier-app', trackNetwork: false };
const tick = (ms = 10) => new Promise((r) => setTimeout(r, ms));

function nativeReplay(started = true) {
  return {
    getContext: async () => null,
    setContext: vi.fn(),
    startReplay: vi.fn(async (_o: unknown) => started),
    stopReplay: vi.fn(async () => true),
    isReplayRecording: vi.fn(async () => true),
    notifyReplayError: vi.fn(),
  };
}

let warn: ReturnType<typeof vi.spyOn>;
beforeEach(() => {
  warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
  globalThis.fetch = (async () => new Response('{}')) as typeof fetch;
});
afterEach(() => {
  vi.restoreAllMocks();
  delete (globalThis as { ErrorUtils?: unknown }).ErrorUtils;
});

describe('session replay (native SDK)', () => {
  it('startReplay forwards only valid options to the TurboModule and resolves true', async () => {
    const { rn, sdk } = await load();
    const m = nativeReplay();
    (rn.TurboModuleRegistry as unknown as { modules: Record<string, unknown> }).modules.TrooperRum = m;
    sdk.init(OPTIONS);
    await expect(sdk.startReplay({ sampleRate: 0.25, onErrorSampleRate: 1, maskAllText: false })).resolves.toBe(true);
    expect(m.startReplay).toHaveBeenLastCalledWith({ sampleRate: 0.25, onErrorSampleRate: 1, maskAllText: false });
    await sdk.startReplay({ sampleRate: 2, onErrorSampleRate: -1, maskAllText: 'no' as unknown as boolean });
    expect(m.startReplay).toHaveBeenLastCalledWith({});
    await sdk.startReplay();
    expect(m.startReplay).toHaveBeenLastCalledWith({});
    await expect(sdk.isReplayRecording()).resolves.toBe(true);
    await expect(sdk.stopReplay()).resolves.toBe(true);
    expect(m.stopReplay).toHaveBeenCalledTimes(1);
    expect(warn).not.toHaveBeenCalled();
    await sdk.close();
    delete (rn.TurboModuleRegistry as unknown as { modules: Record<string, unknown> }).modules.TrooperRum;
  });

  it('works through the legacy NativeModules.TrooperRum, also before init()', async () => {
    const { rn, sdk } = await load();
    const m = nativeReplay();
    rn.NativeModules.TrooperRum = m;
    await expect(sdk.startReplay({ sampleRate: 0.1 })).resolves.toBe(true);
    expect(m.startReplay).toHaveBeenCalledWith({ sampleRate: 0.1 });
    delete rn.NativeModules.TrooperRum;
  });

  it('JS errors (global handler and captureError) trigger the native on-error flush once replay is started', async () => {
    const { rn, sdk } = await load();
    const m = nativeReplay();
    rn.NativeModules.TrooperRum = m;
    let handler: (e: unknown, fatal?: boolean) => void = () => undefined;
    (globalThis as { ErrorUtils?: unknown }).ErrorUtils = { getGlobalHandler: () => handler, setGlobalHandler: (h: typeof handler) => void (handler = h) };
    sdk.init(OPTIONS);
    sdk.captureError(new Error('before replay'));
    expect(m.notifyReplayError).not.toHaveBeenCalled();
    await sdk.startReplay();
    sdk.captureError(new Error('handled'));
    handler(new TypeError('boom'), false);
    expect(m.notifyReplayError).toHaveBeenCalledTimes(2);
    sdk.track('not-an-error');
    expect(m.notifyReplayError).toHaveBeenCalledTimes(2);
    await sdk.stopReplay();
    sdk.captureError(new Error('after stop'));
    expect(m.notifyReplayError).toHaveBeenCalledTimes(2);
    await sdk.close();
    delete rn.NativeModules.TrooperRum;
  });

  it('errors of a session sampled out of RUM do not flush the native buffer', async () => {
    const { rn, sdk } = await load();
    const m = nativeReplay();
    rn.NativeModules.TrooperRum = m;
    sdk.init({ ...OPTIONS, sampleRate: 0 });
    await sdk.startReplay();
    sdk.captureError(new Error('dropped'));
    expect(m.notifyReplayError).not.toHaveBeenCalled();
    await sdk.close();
    delete rn.NativeModules.TrooperRum;
  });

  it('without the native SDK: no-op, resolves false, warns once', async () => {
    const { sdk } = await load();
    sdk.init(OPTIONS);
    await expect(sdk.startReplay()).resolves.toBe(false);
    await expect(sdk.startReplay({ sampleRate: 1 })).resolves.toBe(false);
    expect(warn).toHaveBeenCalledTimes(1);
    expect(String(warn.mock.calls[0]![0])).toContain('startReplay() is a no-op');
    await expect(sdk.isReplayRecording()).resolves.toBe(false);
    await expect(sdk.stopReplay()).resolves.toBe(false);
    expect(() => sdk.captureError(new Error('x'))).not.toThrow();
    await sdk.close();
  });

  it('native module without the replay module (resolves false) or old module without the methods: warns once', async () => {
    const { rn, sdk } = await load();
    rn.NativeModules.TrooperRum = nativeReplay(false);
    await expect(sdk.startReplay()).resolves.toBe(false);
    rn.NativeModules.TrooperRum = { getContext: async () => null };
    await expect(sdk.startReplay()).resolves.toBe(false);
    expect(warn).toHaveBeenCalledTimes(1);
    delete rn.NativeModules.TrooperRum;
  });

  it('throwing / rejecting native methods are swallowed', async () => {
    const { rn, sdk } = await load();
    rn.NativeModules.TrooperRum = {
      startReplay: () => {
        throw new Error('sync');
      },
      stopReplay: () => Promise.reject(new Error('async')),
      isReplayRecording: () => Promise.reject(new Error('async')),
    };
    await expect(sdk.startReplay()).resolves.toBe(false);
    await expect(sdk.stopReplay()).resolves.toBe(false);
    await expect(sdk.isReplayRecording()).resolves.toBe(false);
    await tick();
    delete rn.NativeModules.TrooperRum;
  });

  it('respects nativeBridge: null', async () => {
    const { rn, sdk } = await load();
    const m = nativeReplay();
    rn.NativeModules.TrooperRum = m;
    sdk.init({ ...OPTIONS, nativeBridge: null });
    await expect(sdk.startReplay()).resolves.toBe(false);
    expect(m.startReplay).not.toHaveBeenCalled();
    await sdk.close();
    delete rn.NativeModules.TrooperRum;
  });
});
