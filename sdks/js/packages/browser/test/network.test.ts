import { afterEach, describe, expect, it } from 'vitest';
import { setup, teardown } from './helpers';

afterEach(teardown);

describe('network instrumentation', () => {
  it('fetch: RESOURCE event, traceparent only for targets, ingest calls ignored', async () => {
    const { events, apiCalls } = setup({ tracePropagationTargets: ['https://api.shop.com', /\/internal\//] });
    await fetch('https://api.shop.com/v1/orders/42?token=secret', { method: 'POST', body: '{"a":1}' });
    await fetch('https://cdn.other.com/img.png');
    await fetch('/internal/ping');
    await new Promise((r) => setTimeout(r, 0));
    const tp = (i: number) => new Headers(apiCalls[i]!.init?.headers).get('traceparent');
    expect(tp(0)).toMatch(/^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/);
    expect(tp(1)).toBeNull();
    expect(tp(2)).toMatch(/^00-/);
    const bg = (i: number) => new Headers(apiCalls[i]!.init?.headers).get('baggage');
    expect(bg(0)).toBe('tt.svc=shop-web');
    expect(bg(1)).toBeNull();
    expect(bg(2)).toBe('tt.svc=shop-web');
    const res = (await events()).filter((e) => e.type === 'RESOURCE');
    expect(res).toHaveLength(3); // the /v1/rum flush itself is not instrumented
    expect(res[0]!.resource).toMatchObject({
      url: 'https://api.shop.com/v1/orders/42?token=', method: 'POST', status_code: 200, request_bytes: 7, response_bytes: 2, initiator: 'fetch',
    });
    expect(tp(0)).toBe(`00-${res[0]!.resource!.trace_id}-${res[0]!.resource!.span_id}-01`);
    expect(res[1]!.resource!.trace_id).toBeUndefined();
  });

  it('default propagation: same-origin only', async () => {
    const { apiCalls } = setup();
    await fetch(`${location.origin}/api/x`);
    await fetch('https://third.party/x');
    expect(new Headers(apiCalls[0]!.init?.headers).get('traceparent')).toMatch(/^00-/);
    expect(new Headers(apiCalls[1]!.init?.headers).get('traceparent')).toBeNull();
  });

  it('XHR: injects traceparent and records status', async () => {
    const headers: Record<string, string> = {};
    class FakeXhr extends EventTarget {
      status = 0;
      responseType = '';
      responseText = 'hello';
      open(_m: string, _u: string) {}
      setRequestHeader(k: string, v: string) {
        headers[k] = v;
      }
      getResponseHeader() {
        return null;
      }
      send() {
        this.status = 404;
        setTimeout(() => this.dispatchEvent(new Event('loadend')), 0);
      }
    }
    const orig = globalThis.XMLHttpRequest;
    globalThis.XMLHttpRequest = FakeXhr as unknown as typeof XMLHttpRequest;
    try {
      const { events } = setup({ tracePropagationTargets: [/api\.shop\.com/] });
      const x = new XMLHttpRequest();
      x.open('get', 'https://api.shop.com/items/1');
      x.send();
      await new Promise((r) => setTimeout(r, 5));
      expect(headers.traceparent).toMatch(/^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/);
      expect(headers.baggage).toBe('tt.svc=shop-web');
      const res = (await events()).find((e) => e.type === 'RESOURCE')!;
      expect(res.resource).toMatchObject({ method: 'GET', status_code: 404, response_bytes: 5, initiator: 'xhr' });
      expect(headers.traceparent).toContain(res.resource!.trace_id!);
    } finally {
      await teardown();
      globalThis.XMLHttpRequest = orig;
    }
  });
});
