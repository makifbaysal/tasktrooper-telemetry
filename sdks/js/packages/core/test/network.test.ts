import { afterEach, describe, expect, it, vi } from 'vitest';
import { instrumentFetch, instrumentXhr } from '../src/network';
import { BAGGAGE_MAX_BYTES, mergeBaggage } from '../src/trace';
import type { EventInput } from '../src/client';

const g = globalThis as { fetch: typeof fetch };
const realFetch = g.fetch;
afterEach(() => {
  g.fetch = realFetch;
});

describe('instrumentFetch', () => {
  it('records RESOURCE events and injects traceparent only for targets', async () => {
    const seen: Headers[] = [];
    g.fetch = vi.fn(async (_i: unknown, init?: RequestInit) => {
      seen.push(new Headers(init?.headers));
      return new Response('hello', { status: 201, headers: { 'content-length': '5' } });
    }) as unknown as typeof fetch;
    const events: EventInput[] = [];
    const restore = instrumentFetch({
      capture: (e) => events.push(e),
      tracePropagationTargets: ['https://api.shop.com'],
      ignoreUrls: ['http://localhost:4318/v1/'],
    });
    await fetch('https://api.shop.com/orders?id=1', { method: 'post', body: 'abc', headers: { a: 'b' } });
    await fetch('https://cdn.other.com/x');
    await fetch('http://localhost:4318/v1/rum', { method: 'POST' });
    await new Promise((r) => setTimeout(r, 0));
    expect(seen[0]!.get('traceparent')).toMatch(/^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/);
    expect(seen[0]!.get('a')).toBe('b');
    expect(seen[1]!.get('traceparent')).toBeNull();
    expect(events).toHaveLength(2);
    const r = events[0]!.resource!;
    expect(r).toMatchObject({ method: 'POST', status_code: 201, request_bytes: 3, response_bytes: 5, initiator: 'fetch' });
    expect(seen[0]!.get('traceparent')).toBe(`00-${r.trace_id}-${r.span_id}-01`);
    expect(events[1]!.resource!.trace_id).toBeUndefined();
    restore();
    expect(g.fetch).not.toBe(realFetch); // our mock is restored, not the real one
  });

  it('does not overwrite an existing traceparent and records network errors as status 0', async () => {
    let hdr: string | null = null;
    g.fetch = vi.fn(async (_i: unknown, init?: RequestInit) => {
      hdr = new Headers(init?.headers).get('traceparent');
      throw new TypeError('Failed to fetch');
    }) as unknown as typeof fetch;
    const events: EventInput[] = [];
    instrumentFetch({ capture: (e) => events.push(e), tracePropagationTargets: [/./] });
    await expect(fetch('https://x.com', { headers: { traceparent: 'mine' } })).rejects.toThrow('Failed to fetch');
    await new Promise((r) => setTimeout(r, 0));
    expect(hdr).toBe('mine');
    expect(events[0]!.resource).toMatchObject({ status_code: 0, trace_id: undefined });
  });

  it('adds baggage tt.svc next to traceparent, merging an existing baggage header', async () => {
    const seen: Headers[] = [];
    g.fetch = vi.fn(async (_i: unknown, init?: RequestInit) => {
      seen.push(new Headers(init?.headers));
      return new Response('');
    }) as unknown as typeof fetch;
    instrumentFetch({ capture: () => undefined, tracePropagationTargets: ['https://api.shop.com'], serviceName: 'shop web' });
    await fetch('https://api.shop.com/a');
    await fetch('https://api.shop.com/b', { headers: { baggage: 'k=v, tt.svc=old;p=1', traceparent: 'mine' } });
    await fetch('https://cdn.other.com/x', { headers: { baggage: 'k=v' } });
    expect(seen[0]!.get('baggage')).toBe('tt.svc=shop%20web');
    expect(seen[1]!.get('baggage')).toBe('tt.svc=shop%20web,k=v');
    expect(seen[1]!.get('traceparent')).toBe('mine');
    expect(seen[2]!.get('baggage')).toBe('k=v');
  });
});

describe('mergeBaggage', () => {
  it('encodes, replaces tt.svc, keeps other members and respects the size limit', () => {
    expect(mergeBaggage(undefined, 'a,b=c;d')).toBe('tt.svc=a%2Cb%3Dc%3Bd');
    expect(mergeBaggage('tt.svc=x, a=1 ,, b=2;prop', 'svc')).toBe('tt.svc=svc,a=1,b=2;prop');
    expect(mergeBaggage('a=1', '')).toBe('a=1');
    expect(mergeBaggage(null, undefined)).toBeUndefined();
    const big = 'k=' + 'x'.repeat(BAGGAGE_MAX_BYTES - 5);
    expect(mergeBaggage(big, 'svc')).toBe(big);
  });
});

describe('instrumentXhr baggage', () => {
  it('merges app baggage with tt.svc into a single header for propagation targets only', () => {
    const calls: [string, string][][] = [];
    class FakeXhr extends EventTarget {
      h: [string, string][] = [];
      open() {
        this.h = [];
      }
      setRequestHeader(k: string, v: string) {
        this.h.push([k, v]);
      }
      send() {
        calls.push(this.h);
      }
    }
    const orig = (globalThis as { XMLHttpRequest?: unknown }).XMLHttpRequest;
    (globalThis as { XMLHttpRequest?: unknown }).XMLHttpRequest = FakeXhr;
    try {
      const restore = instrumentXhr({ capture: () => undefined, tracePropagationTargets: [/api\.shop\.com/], serviceName: 'app' });
      const a = new FakeXhr() as unknown as XMLHttpRequest;
      a.open('GET', 'https://api.shop.com/x');
      a.setRequestHeader('Baggage', 'tt.svc=old,k=v');
      a.send();
      const b = new FakeXhr() as unknown as XMLHttpRequest;
      b.open('GET', 'https://other.com/x');
      b.setRequestHeader('baggage', 'k=v');
      b.send();
      restore();
      expect(calls[0]!.filter(([k]) => k.toLowerCase() === 'baggage')).toEqual([['baggage', 'tt.svc=app,k=v']]);
      expect(calls[0]!.some(([k]) => k === 'traceparent')).toBe(true);
      expect(calls[1]).toEqual([['baggage', 'k=v']]);
    } finally {
      (globalThis as { XMLHttpRequest?: unknown }).XMLHttpRequest = orig;
    }
  });
});
