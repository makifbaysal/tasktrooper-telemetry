import { context, propagation, ROOT_CONTEXT, SpanKind, trace, type Context } from '@opentelemetry/api';
import { RPCType, setRPCMetadata } from '@opentelemetry/core';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { setRoute, setRouteResolver } from '../src/api';
import { trackedSpans } from '../src/entry';
import { rateFromTraceState, TrooperSampler } from '../src/sampler';
import { setupTracing } from './helpers';

let t: ReturnType<typeof setupTracing>;
beforeEach(() => {
  t = setupTracing(0.5);
});
afterEach(async () => {
  await t.teardown();
});

const getter = { keys: (c: Record<string, string>) => Object.keys(c), get: (c: Record<string, string>, k: string) => c[k] };
const upstream = (extra: Record<string, string> = {}) =>
  propagation.extract(ROOT_CONTEXT, { traceparent: '00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01', ...extra }, getter);

function serve(parent: Context, fn: (ctx: Context) => void, attrs: Record<string, string> = { 'http.request.method': 'get' }) {
  const span = t.tracer.startSpan('GET', { kind: SpanKind.SERVER, attributes: attrs }, parent);
  const ctx = trace.setSpan(parent, span);
  context.with(ctx, () => fn(ctx));
  span.end();
}

describe('sampler / tracestate', () => {
  it('writes tt=r:<rate> on roots only', () => {
    const root = t.tracer.startSpan('root');
    expect(root.spanContext().traceState?.get('tt')).toBe('r:0.5');
    const child = t.tracer.startSpan('child', {}, trace.setSpan(ROOT_CONTEXT, root));
    expect(child.spanContext().traceState?.get('tt')).toBe('r:0.5'); // inherited, not rewritten
    child.end();
    root.end();
  });

  it('keeps the upstream rate for remote parents', () => {
    const parent = upstream({ tracestate: 'tt=r:0.25,vendor=x' });
    const res = new TrooperSampler(0.5).shouldSample(parent, '0af7651916cd43dd8448eb211c80319c', 's', SpanKind.SERVER, {}, []);
    expect(res.traceState).toBeUndefined();
  });

  it('parses tt sub-fields', () => {
    const ts = trace.getSpanContext(upstream({ tracestate: 'tt=x:1;r:0.1' }))!.traceState;
    expect(rateFromTraceState(ts)).toBe(0.1);
    expect(rateFromTraceState(undefined)).toBeUndefined();
  });

  it('trooper.sample_rate: own rate on roots, upstream rate on remote-parent entries', () => {
    serve(ROOT_CONTEXT, () => {});
    expect(t.spans()[0]!.attributes['trooper.sample_rate']).toBe(0.5);
    t.exporter.reset();
    serve(upstream({ tracestate: 'tt=r:0.2' }), () => {});
    expect(t.spans()[0]!.attributes['trooper.sample_rate']).toBe(0.2);
    t.exporter.reset();
    serve(upstream(), () => {});
    expect(t.spans()[0]!.attributes['trooper.sample_rate']).toBe(0.5);
  });
});

describe('caller baggage', () => {
  it('reads tt.svc into trooper.caller on server entry spans', () => {
    serve(upstream({ baggage: 'tt.svc=gateway,other=1' }), () => {});
    expect(t.spans()[0]!.attributes['trooper.caller']).toBe('gateway');
  });

  it('injects tt.svc on outgoing calls, overriding the inbound value', () => {
    const out: Record<string, string> = {};
    serve(upstream({ baggage: 'tt.svc=gateway,keep=1' }), (ctx) => {
      propagation.inject(ctx, out);
    });
    expect(out.baggage).toContain('tt.svc=svc-a');
    expect(out.baggage).toContain('keep=1');
    expect(out.baggage).not.toContain('gateway');
    expect(out.traceparent).toMatch(/^00-0af7651916cd43dd8448eb211c80319c-/);
  });
});

describe('entry route', () => {
  it('copies the lazily resolved route to children and sets trooper.route', () => {
    let route: string | undefined;
    serve(ROOT_CONTEXT, () => {
      setRouteResolver(() => route);
      const early = t.tracer.startSpan('early'); // before routing: no entry route yet, filled when it ends
      route = '/users/{id}';
      early.end();
      const db = t.tracer.startSpan('db');
      context.with(trace.setSpan(context.active(), db), () => t.tracer.startSpan('nested').end());
      db.end();
    });
    const server = t.span('GET');
    expect(server.attributes['trooper.route']).toBe('/users/:id');
    expect(server.attributes['http.route']).toBe('/users/{id}');
    expect(server.attributes['trooper.entry.route']).toBe('/users/:id');
    expect(server.attributes['trooper.entry.method']).toBe('GET');
    for (const n of ['early', 'db', 'nested']) {
      expect(t.span(n).attributes['trooper.entry.route']).toBe('/users/:id');
      expect(t.span(n).attributes['trooper.entry.method']).toBe('GET');
    }
    expect(trackedSpans()).toBe(0);
  });

  it('uses RPCMetadata routes from OTel framework instrumentations and http.route', () => {
    const span = t.tracer.startSpan('GET', { kind: SpanKind.SERVER, attributes: { 'http.request.method': 'POST' } });
    const meta = { type: RPCType.HTTP, span } as { type: RPCType.HTTP; span: typeof span; route?: string };
    const ctx = setRPCMetadata(trace.setSpan(ROOT_CONTEXT, span), meta);
    context.with(ctx, () => {
      meta.route = '/orders/:orderId';
      t.tracer.startSpan('child').end();
    });
    span.end();
    expect(t.span('child').attributes['trooper.entry.route']).toBe('/orders/:orderId');
    expect(t.span('GET').attributes['trooper.route']).toBe('/orders/:orderId');
    expect(t.span('GET').attributes['trooper.entry.method']).toBe('POST');
  });

  it('explicit setRoute wins; raw ids are normalized', () => {
    serve(ROOT_CONTEXT, () => {
      setRouteResolver(() => '/ignored');
      setRoute('/v1/items/12345');
    });
    expect(t.span('GET').attributes['trooper.route']).toBe('/v1/items/:id');
  });

  it('gRPC servers use the full method name and GRPC', () => {
    serve(ROOT_CONTEXT, () => t.tracer.startSpan('child').end(), {
      'rpc.system': 'grpc',
      'rpc.service': 'shop.v1.Orders',
      'rpc.method': 'Get',
    });
    expect(t.span('child').attributes['trooper.entry.route']).toBe('/shop.v1.Orders/Get');
    expect(t.span('child').attributes['trooper.entry.method']).toBe('GRPC');
  });

  it('strips query values from URL attributes', () => {
    serve(ROOT_CONTEXT, () => {}, { 'http.request.method': 'GET', 'url.full': 'http://h/a?token=secret&x=1', 'url.query': 'token=secret' });
    expect(t.span('GET').attributes['url.full']).toBe('http://h/a?token&x');
    expect(t.span('GET').attributes['url.query']).toBe('token');
  });

  it('internal roots are not entries', () => {
    t.tracer.startSpan('cron').end();
    expect(t.span('cron').attributes['trooper.sample_rate']).toBeUndefined();
    expect(t.span('cron').attributes['trooper.entry.route']).toBeUndefined();
  });
});
