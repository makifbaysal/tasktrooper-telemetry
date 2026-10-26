import { describe, expect, it } from 'vitest';
import { normalizePath, stripQueryValues } from '../src/url';
import { parseDsn } from '../src/dsn';
import { createTraceContext, shouldPropagate } from '../src/trace';
import { toStringMap } from '../src/util';
import { uuid } from '../src/ids';

describe('stripQueryValues', () => {
  it.each([
    ['https://a.com/p?token=abc&x=1', 'https://a.com/p?token=&x='],
    ['https://u:pw@a.com/p?a', 'https://a.com/p?a='],
    ['/p#/route?q=secret', '/p#/route?q='],
    ['https://a.com/p', 'https://a.com/p'],
    ['', ''],
  ])('%s', (input, out) => expect(stripQueryValues(input)).toBe(out));
});

describe('normalizePath', () => {
  it.each([
    ['/users/123/orders/550e8400-e29b-41d4-a716-446655440000', '/users/:id/orders/:id'],
    ['/products/blue-shoes', '/products/blue-shoes'],
    ['/c/5f2b9a7c1d', '/c/:id'],
    ['/a/01HZY3J8K2M4N5P6Q7R8S9T0VW', '/a/:id'],
    ['/x/?q=1', '/x'],
    ['/', '/'],
  ])('%s', (input, out) => expect(normalizePath(input)).toBe(out));
});

describe('parseDsn', () => {
  it('parses the local dev DSN', () => {
    expect(parseDsn('http://tt_dev_ingest_key@localhost:4318/1')).toEqual({
      key: 'tt_dev_ingest_key', origin: 'http://localhost:4318', projectId: '1', rumEndpoint: 'http://localhost:4318/v1/rum',
    });
  });
  it('keeps a path prefix', () => {
    expect(parseDsn('https://k@ingest.trooper.dev/eu/42').rumEndpoint).toBe('https://ingest.trooper.dev/eu/v1/rum');
  });
  it('rejects invalid DSNs', () => {
    expect(() => parseDsn('https://ingest.trooper.dev/1')).toThrow();
    expect(() => parseDsn('')).toThrow();
  });
});

describe('trace context', () => {
  it('formats a W3C traceparent', () => {
    const tc = createTraceContext();
    expect(tc.traceparent).toMatch(/^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/);
    expect(tc.traceparent).toBe(`00-${tc.traceId}-${tc.spanId}-01`);
    expect(tc.traceId).not.toBe('0'.repeat(32));
  });
  it('matches targets', () => {
    const origin = 'https://shop.com';
    expect(shouldPropagate('/api/x', undefined, origin)).toBe(true);
    expect(shouldPropagate('https://cdn.other.com/x', undefined, origin)).toBe(false);
    expect(shouldPropagate('https://api.shop.com/v1', [/api\.shop\.com/], origin)).toBe(true);
    expect(shouldPropagate('https://evil.com/?api.shop', ['https://api.shop.com'], origin)).toBe(false);
    expect(shouldPropagate('https://api.shop.com/v1/x', ['https://api.shop.com'], origin)).toBe(true);
    expect(shouldPropagate('/same', [], origin)).toBe(false);
  });
});

describe('misc', () => {
  it('uuid v4', () => expect(uuid()).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/));
  it('toStringMap', () => {
    expect(toStringMap({ a: 1, b: true, c: { d: 1 }, e: undefined, f: null })).toEqual({ a: '1', b: 'true', c: '{"d":1}', f: 'null' });
  });
});
