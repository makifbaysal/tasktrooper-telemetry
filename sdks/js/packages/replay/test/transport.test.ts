import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ReplayTransport, MAX_QUEUE } from '../src/transport';
import { CLICK, emit, setup, teardown } from './helpers';

vi.mock('@rrweb/record', async () => (await import('./rrweb-mock')).rrwebMock());

beforeEach(() => {
  vi.stubGlobal('CompressionStream', undefined);
  vi.spyOn(Math, 'random').mockReturnValue(0);
});
afterEach(async () => {
  await teardown();
  vi.unstubAllGlobals();
});

/** A transport wired to a fake IntegrationClient. */
function transport(responses: (Response | Error)[]) {
  const calls: RequestInit[] = [];
  const fetch = vi.fn(async (_url: string, init?: RequestInit) => {
    calls.push(init!);
    const r = responses.shift() ?? new Response('', { status: 200 });
    if (r instanceof Error) throw r;
    return r;
  });
  const reportInternal = vi.fn();
  const t = new ReplayTransport({
    config: { dsn: '', endpoint: 'http://localhost:4318', key: 'k', projectId: '1', service: 's', debug: false },
    fetch,
    backoffDelay: (n: number) => 1000 * 2 ** n,
    classifyStatus: (status: number, ra?: string | null) =>
      status < 300 ? { status: 'ok' } : status === 429 || status >= 500 ? { status: 'retry', retryAfterMs: ra ? Number(ra) * 1000 : undefined } : { status: 'drop', code: status },
    reportInternal,
  } as any);
  return { t, fetch, calls, reportInternal };
}

describe('replay transport', () => {
  it('retries 5xx with backoff, then succeeds', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const { t, fetch } = transport([new Response('', { status: 503 }), new Response('', { status: 502 })]);
    await t.enqueue('{"a":1}');
    expect(fetch).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(999);
    expect(fetch).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(fetch).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(2000);
    expect(fetch).toHaveBeenCalledTimes(3);
    expect(t.stats).toEqual({ sent: 1, dropped: 0, retried: 2 });
  });

  it('honours Retry-After on 429', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const { t, fetch } = transport([new Response('', { status: 429, headers: { 'retry-after': '10' } })]);
    await t.enqueue('{}');
    await vi.advanceTimersByTimeAsync(9999);
    expect(fetch).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(fetch).toHaveBeenCalledTimes(2);
  });

  it('drops on other 4xx without retrying', async () => {
    const { t, fetch } = transport([new Response('', { status: 400 }), new Response('', { status: 413 })]);
    await t.enqueue('{"a":1}');
    await t.enqueue('{"b":2}');
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(t.stats).toEqual({ sent: 0, dropped: 2, retried: 0 });
  });

  it('network errors are retried and never thrown', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const { t, fetch, reportInternal } = transport([new TypeError('Failed to fetch')]);
    await expect(t.enqueue('{}')).resolves.toBeUndefined();
    expect(reportInternal).toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1000);
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(t.stats.sent).toBe(1);
  });

  it('caps the retry queue, dropping the oldest', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const { t } = transport([new Response('', { status: 500 })]);
    for (let i = 0; i < MAX_QUEUE + 5; i++) await t.enqueue(`{"i":${i}}`);
    expect(t.stats.dropped).toBe(5);
  });

  it('sendNow uses keepalive only under the 64 KB budget', () => {
    const { t, calls } = transport([]);
    t.sendNow('x'.repeat(100));
    t.sendNow('x'.repeat(70_000));
    expect(calls.map((c) => c.keepalive)).toEqual([true, false]);
  });

  it('a sync-throwing fetch is swallowed', () => {
    const { t } = transport([]);
    (t as any).ic.fetch = () => {
      throw new Error('blocked');
    };
    expect(() => t.sendNow('{}')).not.toThrow();
  });
});

describe('end to end', () => {
  it('a failing ingest never surfaces to the host page', async () => {
    const { integration, fetchStub } = setup({ sampleRate: 1 }, {}, () => {
      throw new Error('offline');
    });
    emit(CLICK());
    await expect(integration.flush()).resolves.toBeUndefined();
    expect(fetchStub).toHaveBeenCalled();
  });
});
