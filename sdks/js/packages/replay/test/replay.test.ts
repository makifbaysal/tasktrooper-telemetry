import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { captureError, clearUser, identify, track } from '@trooper/browser';
import { SEGMENT_BYTES, SEGMENT_MS } from '../src/index';
import { KEY_PREFIX } from '../src/store';
import { CLICK, emit, INPUT, MUTATION, rr, setup, teardown } from './helpers';

vi.mock('@rrweb/record', async () => (await import('./rrweb-mock')).rrwebMock());

beforeEach(() => {
  vi.stubGlobal('CompressionStream', undefined); // plain JSON bodies unless a test opts into gzip
});
afterEach(async () => {
  await teardown();
  vi.unstubAllGlobals();
});

const random = (v: number) => vi.spyOn(Math, 'random').mockReturnValue(v);
let currentSid = '';
/** Shared per-session entry in localStorage (`tt_replay:<session_id>`). */
const stored = (sid = currentSid) => JSON.parse(localStorage.getItem(KEY_PREFIX + sid) ?? 'null');
const replayKeys = () => Object.keys(localStorage).filter((k) => k.startsWith(KEY_PREFIX));
const track$ = <T extends { sdk: { client: { sessionId(): string } } }>(r: T): T => {
  currentSid = r.sdk.client.sessionId();
  return r;
};

describe('sampling', () => {
  it('records the whole session when sampled by sampleRate', () => {
    random(0.05);
    const { integration } = track$(setup({ sampleRate: 0.1 }));
    expect(integration.getMode()).toBe('session');
    expect(rr.calls).toBe(1);
    expect(stored()).toEqual({ m: 'session', n: 0 });
  });

  it('buffers for errors when only onErrorSampleRate applies', () => {
    random(0.5);
    const { integration } = setup({ sampleRate: 0.1, onErrorSampleRate: 1 });
    expect(integration.getMode()).toBe('buffer');
    expect(rr.calls).toBe(1);
  });

  it('does not record when neither rate applies', () => {
    random(0.5);
    const { integration } = track$(setup({ sampleRate: 0.1, onErrorSampleRate: 0.3 }));
    expect(integration.getMode()).toBe('off');
    expect(rr.calls).toBe(0);
    expect(stored().m).toBe('off');
  });

  it('defaults: 0.1 session, 1.0 on error', () => {
    random(0.09);
    expect(setup().integration.getMode()).toBe('session');
  });

  it('a decision stored for the session (another tab / earlier page load) is used without re-rolling', () => {
    // persist:true keeps the session in localStorage, so a second init continues it.
    random(0.05);
    const a = setup({}, { persist: true });
    const sid = a.sdk.client.sessionId();
    expect(a.integration.getMode()).toBe('session');
    const rumSession = localStorage.getItem('tt_session')!;
    return teardown().then(() => {
      localStorage.setItem('tt_session', rumSession); // next page load of the same session
      localStorage.setItem(KEY_PREFIX + sid, JSON.stringify({ m: 'buffer', n: 3 }));
      const rnd = random(0.0);
      const b = setup({}, { persist: true });
      expect(b.sdk.client.sessionId()).toBe(sid);
      expect(b.integration.getMode()).toBe('buffer');
      expect(rnd).not.toHaveBeenCalled();
      localStorage.clear();
    });
  });

  it('does not record sessions sampled out of RUM', () => {
    random(0.0);
    const { integration } = setup({ sampleRate: 1 }, { sampleRate: 0 });
    expect(integration.getMode()).toBe('off');
    expect(rr.calls).toBe(0);
  });

  it('enabled: false turns replay off entirely', () => {
    random(0.0);
    const { integration } = setup({ enabled: false, sampleRate: 1 });
    expect(integration.getMode()).toBe('off');
    expect(rr.calls).toBe(0);
  });
});

describe('session mode segments', () => {
  it('payload matches contracts §23', async () => {
    random(0.0);
    const { sdk, integration, sent, settle } = track$(setup({ sampleRate: 1 }));
    identify('user-42');
    emit(CLICK(5));
    emit(CLICK(5));
    emit(INPUT(7));
    emit(INPUT(7)); // same field: one input interaction
    emit(INPUT(8));
    await integration.flush();
    await settle();
    expect(sent).toHaveLength(1);
    const { url, init, payload } = sent[0]!;
    expect(url).toBe('http://localhost:4318/v1/replay?key=tt_dev_ingest_key');
    expect(init.headers).toEqual({ 'content-type': 'text/plain;charset=UTF-8' });
    expect(init.credentials).toBe('omit');
    expect(Object.keys(payload).sort()).toEqual(
      ['resource', 'session_id', 'segment', 'start_ts', 'end_ts', 'format', 'has_full_snapshot', 'view_id', 'view_name', 'user_id', 'anonymous_id', 'stats', 'events'].sort(),
    );
    expect(payload).toMatchObject({
      resource: { service: 'shop-web', env: 'production', version: '1.4.0', platform: 'web', sdk_name: '@trooper/replay', sdk_version: '0.1.0' },
      session_id: sdk.client.sessionId(),
      segment: 0,
      format: 'rrweb',
      has_full_snapshot: true,
      view_id: sdk.client.view!.id,
      view_name: '/',
      user_id: 'user-42',
      anonymous_id: sdk.client.anonymousId,
      stats: { clicks: 2, errors: 0, rage_clicks: 0, inputs: 2 },
    });
    expect(payload.events.map((e: any) => e.type)).toEqual([4, 2, 3, 3, 3, 3, 3]);
    expect(payload.start_ts).toBe((payload.events[0] as any).timestamp);
    expect(payload.end_ts).toBe((payload.events[6] as any).timestamp);
    expect(stored().n).toBe(1);
  });

  it('flushes after 5 s and numbers segments from 0', async () => {
    random(0.0);
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
    const { sent, settle } = setup({ sampleRate: 1 });
    await vi.advanceTimersByTimeAsync(SEGMENT_MS - 1);
    expect(sent).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(1);
    emit(CLICK());
    await vi.advanceTimersByTimeAsync(SEGMENT_MS);
    vi.useRealTimers();
    await settle();
    expect(sent.map((s) => s.payload.segment)).toEqual([0, 1]);
    expect(sent[0]!.payload.has_full_snapshot).toBe(true);
    expect(sent[1]!.payload.has_full_snapshot).toBe(false);
    expect(sent[1]!.payload.stats.clicks).toBe(1);
  });

  it('flushes immediately once ~256 KB are collected', async () => {
    random(0.0);
    const { sent, settle } = setup({ sampleRate: 1 });
    emit(MUTATION(SEGMENT_BYTES / 2));
    expect(sent).toHaveLength(0);
    emit(MUTATION(SEGMENT_BYTES / 2));
    await settle();
    expect(sent).toHaveLength(1);
    expect(sent[0]!.payload.events).toHaveLength(4);
  });

  it('checkout (every 5 min) starts a new segment with a FullSnapshot', async () => {
    random(0.0);
    const { integration, sent, settle } = setup({ sampleRate: 1 });
    emit(CLICK(1, Date.now() + 5 * 60 * 1000 + 1));
    await new Promise((r) => setTimeout(r, 5));
    expect(rr.snapshots).toBe(1);
    await integration.flush();
    await settle();
    expect(sent.map((s) => [s.payload.segment, s.payload.has_full_snapshot, s.payload.events.length])).toEqual([
      [0, true, 3],
      [1, true, 2],
    ]);
  });

  it('counts errors and rage clicks from RUM events', async () => {
    random(0.0);
    const { sdk, integration, sent, settle } = setup({ sampleRate: 1 });
    captureError(new Error('boom'));
    sdk.client.capture({ type: 'ACTION', action: { name: 'Pay', rage: true } });
    sdk.client.capture({ type: 'ACTION', action: { name: 'Pay', rage: false } });
    await integration.flush();
    await settle();
    expect(sent[0]!.payload.stats).toMatchObject({ errors: 1, rage_clicks: 1 });
  });

  it('pagehide sends the open segment with fetch keepalive, split under 64 KB', async () => {
    random(0.0);
    const { sent, settle } = setup({ sampleRate: 1 });
    for (let i = 0; i < 3; i++) emit(MUTATION(25_000));
    window.dispatchEvent(new Event('pagehide'));
    await settle();
    expect(sent.length).toBe(2);
    for (const s of sent) {
      expect(s.init.keepalive).toBe(true);
      expect(String(s.init.body).length).toBeLessThanOrEqual(60_000);
      expect(s.init.headers).toEqual({ 'content-type': 'text/plain;charset=UTF-8' });
    }
    expect(sent.map((s) => s.payload.segment)).toEqual([0, 1]);
    expect(sent.flatMap((s) => s.payload.events)).toHaveLength(5);
  });

  it('gzip with CompressionStream when available', async () => {
    vi.unstubAllGlobals();
    expect(typeof CompressionStream).toBe('function');
    random(0.0);
    const { integration, sent, settle } = setup({ sampleRate: 1 });
    await integration.flush();
    await settle();
    expect(sent[0]!.init.headers).toEqual({ 'content-type': 'application/json', 'content-encoding': 'gzip' });
    expect(sent[0]!.payload.has_full_snapshot).toBe(true);
  });
});

describe('buffer mode', () => {
  it('keeps events in memory until an ERROR, then flushes them and keeps recording', async () => {
    random(0.5);
    const { integration, sent, settle } = track$(setup({ sampleRate: 0.1, onErrorSampleRate: 1 }));
    emit(CLICK());
    await integration.flush();
    await settle();
    expect(sent).toHaveLength(0);
    captureError(new Error('checkout failed'));
    await settle();
    expect(integration.getMode()).toBe('session');
    expect(stored().m).toBe('session');
    expect(sent).toHaveLength(1);
    expect(sent[0]!.payload).toMatchObject({ segment: 0, has_full_snapshot: true, stats: { clicks: 1, errors: 1 } });
    emit(CLICK());
    await integration.flush();
    await settle();
    expect(sent.map((s) => s.payload.segment)).toEqual([0, 1]);
  });

  it('checks out every 60 s and keeps only the previous + current window', async () => {
    random(0.5);
    const { sent, settle } = setup({ sampleRate: 0.1, onErrorSampleRate: 1 });
    const t0 = Date.now();
    emit(CLICK(1, t0 + 61_000)); // triggers checkout #1
    await new Promise((r) => setTimeout(r, 5));
    emit(CLICK(2, t0 + 122_000)); // triggers checkout #2 (lastCheckout is real time; any ts ≥ 60 s later works)
    await new Promise((r) => setTimeout(r, 5));
    expect(rr.snapshots).toBe(2);
    captureError(new Error('x'));
    await settle();
    // initial window dropped; previous (snapshot #1 + click 2) and current (snapshot #2) are sent, each with a FullSnapshot.
    expect(sent.map((s) => s.payload.has_full_snapshot)).toEqual([true, true]);
    const clicks = sent.flatMap((s) => s.payload.events).filter((e: any) => e.type === 3).map((e: any) => e.data.id);
    expect(clicks).toEqual([2]);
  });

  it('pagehide in buffer mode sends nothing', async () => {
    random(0.5);
    const { sent, settle } = setup({ sampleRate: 0.1, onErrorSampleRate: 1 });
    window.dispatchEvent(new Event('pagehide'));
    await settle();
    expect(sent).toHaveLength(0);
  });
});

describe('shared state across tabs', () => {
  it('segment numbers continue from the shared counter (read-increment-write at seal)', async () => {
    random(0.0);
    const { sdk, integration, sent, settle } = track$(setup({ sampleRate: 1 }));
    const sid = sdk.client.sessionId();
    // Another tab of the same session sealed segments 0..4 meanwhile.
    localStorage.setItem(KEY_PREFIX + sid, JSON.stringify({ m: 'session', n: 5 }));
    await integration.flush();
    emit(CLICK());
    await integration.flush();
    await settle();
    expect(sent.map((s) => s.payload.segment)).toEqual([5, 6]);
    expect(stored()).toEqual({ m: 'session', n: 7 });
  });

  it('each tab starts with its own FullSnapshot even when joining a recorded session', async () => {
    random(0.0);
    const a = track$(setup({ sampleRate: 1 }, { persist: true }));
    const sid = a.sdk.client.sessionId();
    const rumSession = localStorage.getItem('tt_session')!;
    await teardown();
    localStorage.setItem('tt_session', rumSession); // second tab of the same session
    localStorage.setItem(KEY_PREFIX + sid, JSON.stringify({ m: 'session', n: 2 }));
    random(0.99);
    const b = setup({ sampleRate: 0 }, { persist: true }); // would be 'off' if it re-rolled
    expect(b.sdk.client.sessionId()).toBe(sid);
    expect(b.integration.getMode()).toBe('session');
    await b.integration.flush();
    await b.settle();
    expect(b.sent.map((s) => [s.payload.segment, s.payload.has_full_snapshot])).toEqual([[2, true]]);
    localStorage.clear();
  });

  it('an error in a buffering tab switches the shared decision to session', async () => {
    random(0.5);
    const { sdk } = track$(setup({ sampleRate: 0.1, onErrorSampleRate: 1 }));
    expect(stored()).toEqual({ m: 'buffer', n: 0 });
    captureError(new Error('x'));
    expect(stored(sdk.client.sessionId())).toEqual({ m: 'session', n: 1 });
  });

  it('a storage event switching this session to `session` makes a buffering tab flush and keep recording', async () => {
    random(0.5);
    const { sdk, integration, sent, settle } = track$(setup({ sampleRate: 0.1, onErrorSampleRate: 1 }));
    const sid = sdk.client.sessionId();
    emit(CLICK());
    const storage = (key: string | null, newValue: string | null) =>
      window.dispatchEvent(new StorageEvent('storage', { key, newValue, storageArea: localStorage }));
    // Ignored: other session, other key, cleared storage, unparsable or non-session values.
    storage(KEY_PREFIX + 'other-session', JSON.stringify({ m: 'session', n: 1 }));
    storage('tt_session', 'x');
    storage(null, null);
    localStorage.setItem(KEY_PREFIX + sid, '{');
    storage(KEY_PREFIX + sid, '{');
    localStorage.setItem(KEY_PREFIX + sid, JSON.stringify({ m: 'buffer', n: 0 }));
    storage(KEY_PREFIX + sid, JSON.stringify({ m: 'buffer', n: 0 }));
    await settle();
    expect(integration.getMode()).toBe('buffer');
    expect(sent).toHaveLength(0);
    // Another tab hit an ERROR and wrote the shared decision (and sealed segment 0).
    localStorage.setItem(KEY_PREFIX + sid, JSON.stringify({ m: 'session', n: 1 }));
    storage(KEY_PREFIX + sid, JSON.stringify({ m: 'session', n: 1 }));
    await settle();
    expect(integration.getMode()).toBe('session');
    expect(sent).toHaveLength(1);
    expect(sent[0]!.payload).toMatchObject({ session_id: sid, segment: 1, has_full_snapshot: true, stats: { clicks: 1, errors: 0 } });
    expect(sent[0]!.payload.events.map((e: any) => e.type).slice(0, 2)).toEqual([4, 2]); // Meta + FullSnapshot first
    emit(CLICK());
    await integration.flush();
    await settle();
    expect(sent.map((s) => s.payload.segment)).toEqual([1, 2]);
    expect(stored()).toEqual({ m: 'session', n: 3 });
    storage(KEY_PREFIX + sid, JSON.stringify({ m: 'session', n: 3 })); // already recording: no-op
    await settle();
    expect(sent).toHaveLength(2);
  });

  it('stops listening to storage events after stop()', async () => {
    random(0.5);
    const { sdk, integration, sent, settle } = setup({ sampleRate: 0.1, onErrorSampleRate: 1 });
    const sid = sdk.client.sessionId();
    emit(CLICK());
    await integration.stop();
    localStorage.setItem(KEY_PREFIX + sid, JSON.stringify({ m: 'session', n: 0 }));
    window.dispatchEvent(new StorageEvent('storage', { key: KEY_PREFIX + sid, newValue: JSON.stringify({ m: 'session', n: 0 }) }));
    await settle();
    expect(sent).toHaveLength(0);
    expect(integration.getMode()).toBe('off');
  });

  it('removes entries of old sessions on start', () => {
    localStorage.setItem(KEY_PREFIX + 'old-1', JSON.stringify({ m: 'session', n: 9 }));
    localStorage.setItem('unrelated', '1');
    random(0.0);
    const { sdk } = setup({ sampleRate: 1 });
    expect(replayKeys()).toEqual([KEY_PREFIX + sdk.client.sessionId()]);
    expect(localStorage.getItem('unrelated')).toBe('1');
  });

  it('falls back to sessionStorage, then memory', async () => {
    const { ReplayStore } = await import('../src/store');
    const ls = vi.spyOn(window, 'localStorage', 'get').mockImplementation(() => {
      throw new Error('blocked');
    });
    const st = new ReplayStore();
    expect(st.kind).toBe('session');
    expect(st.takeSegment('s1', 'session')).toBe(0);
    expect(st.takeSegment('s1', 'session')).toBe(1);
    expect(JSON.parse(sessionStorage.getItem(KEY_PREFIX + 's1')!)).toEqual({ m: 'session', n: 2 });
    vi.spyOn(window, 'sessionStorage', 'get').mockImplementation(() => {
      throw new Error('blocked');
    });
    const mem = new ReplayStore();
    expect(mem.kind).toBe('memory');
    expect(mem.takeSegment('s1', 'buffer')).toBe(0);
    expect(mem.takeSegment('s1', 'buffer')).toBe(1);
    expect(mem.get('s1')).toEqual({ m: 'buffer', n: 2 });
    mem.cleanup('s2');
    expect(mem.get('s1')).toBeUndefined();
    ls.mockRestore();
  });
});

describe('session rotation', () => {
  it('flushes the old session and starts a new one with a FullSnapshot at segment 0', async () => {
    random(0.0);
    const { sdk, integration, sent, settle } = setup({ sampleRate: 1 });
    const oldId = sdk.client.sessionId();
    emit(CLICK());
    clearUser();
    track('after-logout'); // first event of the new session → SESSION_START → rotation hook
    const newId = sdk.client.sessionId();
    expect(newId).not.toBe(oldId);
    expect(rr.snapshots).toBe(1);
    await integration.flush();
    await settle();
    expect(sent.map((s) => [s.payload.session_id, s.payload.segment, s.payload.has_full_snapshot])).toEqual([
      [oldId, 0, true],
      [newId, 0, true],
    ]);
    expect(stored(newId)).toEqual({ m: 'session', n: 1 });
    expect(stored(oldId)).toBeNull(); // previous session's entry cleaned up
    expect(replayKeys()).toEqual([KEY_PREFIX + newId]);
  });

  it('stops recording when the new session is not sampled', () => {
    const rnd = random(0.0);
    const { integration } = setup({ sampleRate: 0.5, onErrorSampleRate: 0 });
    rnd.mockReturnValue(0.9);
    clearUser();
    track('x');
    expect(integration.getMode()).toBe('off');
    expect(rr.stops).toBe(1);
  });
});

describe('never throws into the host', () => {
  it('rrweb failing to start is swallowed', () => {
    random(0.0);
    rr.throwOnRecord = true;
    let r: ReturnType<typeof setup> | undefined;
    expect(() => (r = setup({ sampleRate: 1 }))).not.toThrow();
    expect(r!.integration.getMode()).toBe('off');
    expect(() => track('still works')).not.toThrow();
  });

  it('garbage events and broken storage are tolerated', async () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('denied');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('denied');
    });
    random(0.0);
    const { integration } = setup({ sampleRate: 1 });
    expect(integration.getMode()).toBe('session');
    const circular: any = { type: 3, timestamp: 1 };
    circular.self = circular;
    expect(() => emit(circular)).not.toThrow();
    expect(() => emit(null)).not.toThrow();
    await expect(integration.flush()).resolves.toBeUndefined();
  });
});
