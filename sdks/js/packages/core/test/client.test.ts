import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Client, type ClientConfig } from '../src/client';
import { MemoryOfflineStore } from '../src/offline';
import { backoffDelay, classifyStatus, parseRetryAfter, type SendResult, type Transport } from '../src/transport';
import type { RumBatch } from '../src/types';

function mockTransport(results: SendResult[] = []) {
  const bodies: RumBatch[] = [];
  const beacons: RumBatch[] = [];
  const send = vi.fn(async (b: string, o?: { beacon?: boolean }) => {
    (o?.beacon ? beacons : bodies).push(JSON.parse(b));
    return results.shift() ?? { status: 'ok' as const };
  });
  return { transport: { send } as Transport, bodies, beacons, send };
}

const make = (over: Partial<ClientConfig> = {}) =>
  new Client({ dsn: 'http://k@localhost:4318/1', app: 'app', platform: 'PLATFORM_WEB', sdk: { name: 't', version: '0' }, ...over });

const flushPromises = async () => {
  for (let i = 0; i < 10; i++) await Promise.resolve();
};

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe('batching', () => {
  it('flushes after 5 s', async () => {
    const t = mockTransport();
    const c = make({ transport: t.transport });
    c.capture({ type: 'CUSTOM', custom: { name: 'x' } });
    await vi.advanceTimersByTimeAsync(4999);
    expect(t.send).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(t.bodies).toHaveLength(1);
    expect(t.bodies[0]!.events.map((e) => e.type)).toEqual(['SESSION_START', 'CUSTOM']);
  });

  it('flushes immediately at 100 events and never exceeds 100 per batch', async () => {
    const t = mockTransport();
    const c = make({ transport: t.transport });
    for (let i = 0; i < 250; i++) c.capture({ type: 'CUSTOM', custom: { name: String(i) } });
    await flushPromises();
    expect(t.bodies.map((b) => b.events.length)).toEqual([100, 100]);
    await vi.advanceTimersByTimeAsync(5000);
    expect(t.bodies.map((b) => b.events.length)).toEqual([100, 100, 51]);
  });

  it('enriches events with session, view, user, connection and scrubs URLs', async () => {
    const t = mockTransport();
    const c = make({ transport: t.transport, getConnection: () => 'wifi' });
    c.view = { id: 'v1', name: '/p/:id' };
    c.setUser('42', { plan: 'pro', n: 3 });
    c.capture({ type: 'RESOURCE', resource: { url: 'https://api.x/p?secret=1' } });
    await c.flush();
    const e = t.bodies[0]!.events[1]!;
    expect(e).toMatchObject({ view_id: 'v1', view_name: '/p/:id', connection: 'wifi', user: { id: '42', traits: { plan: 'pro', n: '3' } } });
    expect(e.resource!.url).toBe('https://api.x/p?secret=');
    expect(e.session_id).toBe(t.bodies[0]!.events[0]!.session_id);
    expect(e.anonymous_id).toBeTruthy();
  });
});

describe('beforeSend & sampling', () => {
  it('can modify and drop events; a throwing hook drops (fail closed)', async () => {
    const t = mockTransport();
    const c = make({
      transport: t.transport,
      beforeSend: (e) => {
        if (e.custom?.name === 'drop') return null;
        if (e.custom?.name === 'boom') throw new Error('x');
        if (e.custom) e.custom.name = 'renamed';
        return e;
      },
    });
    c.capture({ type: 'CUSTOM', custom: { name: 'keep' } });
    c.capture({ type: 'CUSTOM', custom: { name: 'drop' } });
    c.capture({ type: 'CUSTOM', custom: { name: 'boom' } });
    await c.flush();
    expect(t.bodies[0]!.events.filter((e) => e.type === 'CUSTOM').map((e) => e.custom!.name)).toEqual(['renamed']);
  });

  it('sampleRate 0 records nothing', async () => {
    const t = mockTransport();
    const c = make({ transport: t.transport, sampleRate: 0 });
    c.capture({ type: 'CUSTOM', custom: { name: 'x' } });
    await c.flush();
    expect(t.send).not.toHaveBeenCalled();
    expect(c.stats.sampledOut).toBe(1);
  });

  it('enabled:false is a no-op', () => {
    const c = make({ enabled: false });
    expect(c.capture({ type: 'CUSTOM', custom: { name: 'x' } })).toBeUndefined();
  });
});

describe('retry / backoff', () => {
  it('classifies statuses per contract', () => {
    expect(classifyStatus(200)).toEqual({ status: 'ok' });
    expect(classifyStatus(429, '3')).toEqual({ status: 'retry', retryAfterMs: 3000 });
    expect(classifyStatus(503)).toEqual({ status: 'retry' });
    expect(classifyStatus(400)).toEqual({ status: 'drop', code: 400 });
    expect(classifyStatus(401)).toEqual({ status: 'drop', code: 401 });
    expect(classifyStatus(413)).toEqual({ status: 'drop', code: 413 });
    expect(parseRetryAfter('Wed, 21 Oct 2015 07:28:10 GMT', Date.parse('Wed, 21 Oct 2015 07:28:00 GMT'))).toBe(10000);
  });

  it('backoff grows 1 s → 60 s with jitter', () => {
    expect(backoffDelay(0, () => 1)).toBe(1000);
    expect(backoffDelay(0, () => 0)).toBe(500);
    expect(backoffDelay(3, () => 1)).toBe(8000);
    expect(backoffDelay(10, () => 1)).toBe(60000);
    expect(backoffDelay(100, () => 0)).toBe(30000);
  });

  it('queues on 5xx, retries with backoff, keeps order, then succeeds', async () => {
    vi.spyOn(Math, 'random').mockReturnValue(1);
    const t = mockTransport([{ status: 'retry' }, { status: 'retry' }]);
    const store = new MemoryOfflineStore();
    const c = make({ transport: t.transport, offlineStore: store });
    c.capture({ type: 'CUSTOM', custom: { name: 'a' } });
    await c.flush();
    expect(await store.size()).toBe(1);
    c.capture({ type: 'CUSTOM', custom: { name: 'b' } });
    await c.flush(); // in backoff → queued, not sent
    expect(t.send).toHaveBeenCalledTimes(1);
    expect(await store.size()).toBe(2);
    await vi.advanceTimersByTimeAsync(1000); // attempt 1 fails again
    expect(t.send).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(1999);
    expect(t.send).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(1); // 2 s later: drains both, oldest first
    expect(t.send).toHaveBeenCalledTimes(4);
    expect(t.bodies.slice(2).map((b) => b.events.at(-1)!.custom!.name)).toEqual(['a', 'b']);
    expect(await store.size()).toBe(0);
    vi.restoreAllMocks();
  });

  it('honours Retry-After on 429', async () => {
    const t = mockTransport([{ status: 'retry', retryAfterMs: 30000 }]);
    const c = make({ transport: t.transport });
    c.capture({ type: 'CUSTOM', custom: { name: 'a' } });
    await c.flush();
    await vi.advanceTimersByTimeAsync(29999);
    expect(t.send).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(t.send).toHaveBeenCalledTimes(2);
  });

  it('drops the batch on 4xx', async () => {
    const t = mockTransport([{ status: 'drop', code: 400 }]);
    const store = new MemoryOfflineStore();
    const c = make({ transport: t.transport, offlineStore: store });
    c.capture({ type: 'CUSTOM', custom: { name: 'a' } });
    await c.flush();
    expect(await store.size()).toBe(0);
    expect(c.stats.dropped).toBe(1);
    await vi.advanceTimersByTimeAsync(120000);
    expect(t.send).toHaveBeenCalledTimes(1);
  });

  it('queues while offline and drains when back online', async () => {
    let online = false;
    const t = mockTransport();
    const c = make({ transport: t.transport, isOnline: () => online });
    c.capture({ type: 'CUSTOM', custom: { name: 'a' } });
    await c.flush();
    expect(t.send).not.toHaveBeenCalled();
    online = true;
    await c.drain();
    expect(t.send).toHaveBeenCalledTimes(1);
  });

  it('a throwing transport never escapes', async () => {
    const c = make({ transport: { send: () => Promise.reject(new Error('boom')) } });
    c.capture({ type: 'CUSTOM', custom: { name: 'a' } });
    await expect(c.flush()).resolves.toBeUndefined();
  });
});

describe('beacon flush', () => {
  it('sends synchronously via beacon and splits large payloads under 60 KB', async () => {
    const t = mockTransport();
    const c = make({ transport: t.transport });
    for (let i = 0; i < 90; i++) c.capture({ type: 'CUSTOM', custom: { name: 'x'.repeat(1000) } });
    const p = c.flush({ beacon: true });
    expect(t.send).toHaveBeenCalled(); // before awaiting
    await p;
    expect(t.beacons.length).toBeGreaterThan(1);
    expect(t.beacons.reduce((n, b) => n + b.events.length, 0)).toBe(91);
  });

  it('persists a failed beacon to the offline store', async () => {
    const t = mockTransport([{ status: 'retry' }]);
    const store = new MemoryOfflineStore();
    const c = make({ transport: t.transport, offlineStore: store });
    c.capture({ type: 'CUSTOM', custom: { name: 'a' } });
    await c.flush({ beacon: true });
    expect(await store.size()).toBe(1);
  });
});

describe('session rotation', () => {
  it('emits SESSION_START for a new session after 30 min idle and notifies', async () => {
    vi.setSystemTime(0);
    const t = mockTransport();
    const starts: boolean[] = [];
    const c = make({ transport: t.transport, onSessionStart: (_s, first) => starts.push(first) });
    c.capture({ type: 'CUSTOM', custom: { name: 'a' } });
    vi.setSystemTime(31 * 60 * 1000);
    c.capture({ type: 'CUSTOM', custom: { name: 'b' } });
    await c.flush();
    const ev = t.bodies[0]!.events;
    expect(ev.map((e) => e.type)).toEqual(['SESSION_START', 'CUSTOM', 'SESSION_START', 'CUSTOM']);
    expect(ev[0]!.session_id).not.toBe(ev[2]!.session_id);
    expect(ev[3]!.session_id).toBe(ev[2]!.session_id);
    expect(starts).toEqual([true, false]);
  });
});

describe('offline store', () => {
  it('caps at max bytes dropping oldest (FIFO)', async () => {
    const s = new MemoryOfflineStore(10);
    await s.push('aaaa');
    await s.push('bbbb');
    await s.push('cccc');
    expect(await s.size()).toBe(2);
    expect(await s.peek()).toBe('bbbb');
    await s.push('x'.repeat(11)); // larger than the cap: ignored
    expect(await s.size()).toBe(2);
  });
});

describe('drain race', () => {
  it('a batch flushed while a drain is finishing is not stranded in the queue', async () => {
    const t = mockTransport();
    const store = new MemoryOfflineStore();
    await store.push(JSON.stringify({ resource: {}, events: [{ type: 'CUSTOM' }] }));
    const c = make({ transport: t.transport, offlineStore: store });
    const d = c.drain();
    c.capture({ type: 'CUSTOM', custom: { name: 'late' } });
    await c.flush();
    await d;
    expect(await store.size()).toBe(0);
    expect(t.send).toHaveBeenCalledTimes(2);
  });
});

describe('adoptIdentity', () => {
  it('rewrites buffered events to the adopted session / anonymous id', async () => {
    const t = mockTransport();
    const c = make({ transport: t.transport });
    c.capture({ type: 'CUSTOM', custom: { name: 'a' } });
    c.adoptIdentity({ sessionId: 'native-session', anonymousId: 'native-anon' });
    c.capture({ type: 'CUSTOM', custom: { name: 'b' } });
    await c.flush();
    const ev = t.bodies[0]!.events;
    expect(new Set(ev.map((e) => e.session_id))).toEqual(new Set(['native-session']));
    expect(new Set(ev.map((e) => e.anonymous_id))).toEqual(new Set(['native-anon']));
    expect(ev.filter((e) => e.type === 'SESSION_START')).toHaveLength(1);
  });
});
