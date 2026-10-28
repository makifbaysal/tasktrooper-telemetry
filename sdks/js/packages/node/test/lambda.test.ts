import { context, ROOT_CONTEXT, SpanKind, SpanStatusCode, trace } from '@opentelemetry/api';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { injectHeaders } from '../src/messaging';
import { inferTrigger, resetColdStart, wrapLambda } from '../src/lambda';
import { internalErrorCount } from '../src/errors';
import { setupTracing } from './helpers';

const lctx = { awsRequestId: 'req-1', functionName: 'ctx-name', invokedFunctionArn: 'arn:aws:lambda:eu-central-1:1:function:orders-api' };

const apiV2 = {
  version: '2.0',
  routeKey: 'GET /users/{id}',
  rawPath: '/users/42',
  headers: {} as Record<string, string>,
  requestContext: { http: { method: 'GET', path: '/users/42' } },
};
const apiV1 = { resource: '/orders/{orderId}', path: '/orders/9', httpMethod: 'POST', headers: {}, requestContext: { stage: 'prod' } };
const sqs = { Records: [{ eventSource: 'aws:sqs', eventSourceARN: 'arn:aws:sqs:eu-central-1:1:orders', body: '{}' }, { eventSource: 'aws:sqs' }] };

describe('inferTrigger', () => {
  it('maps event shapes to faas.trigger', () => {
    expect(inferTrigger(apiV2)).toBe('http');
    expect(inferTrigger(apiV1)).toBe('http');
    expect(inferTrigger({ requestContext: { elb: { targetGroupArn: 'x' } }, httpMethod: 'GET' })).toBe('http');
    expect(inferTrigger(sqs)).toBe('pubsub');
    expect(inferTrigger({ Records: [{ EventSource: 'aws:sns', Sns: { TopicArn: 'arn:aws:sns:r:1:t' } }] })).toBe('pubsub');
    expect(inferTrigger({ Records: [{ eventSource: 'aws:kinesis' }] })).toBe('pubsub');
    expect(inferTrigger({ Records: [{ eventSource: 'aws:dynamodb' }] })).toBe('datasource');
    expect(inferTrigger({ Records: [{ eventSource: 'aws:s3', s3: {} }] })).toBe('datasource');
    expect(inferTrigger({ source: 'aws.events', 'detail-type': 'Scheduled Event' })).toBe('timer');
    expect(inferTrigger({ source: 'shop.orders', 'detail-type': 'OrderCreated' })).toBe('pubsub');
    expect(inferTrigger({ eventSource: 'aws:kafka', records: {} })).toBe('pubsub');
    expect(inferTrigger({ foo: 1 })).toBe('other');
    expect(inferTrigger(null)).toBe('other');
    expect(inferTrigger('x')).toBe('other');
  });
});

describe('wrapLambda', () => {
  let t: ReturnType<typeof setupTracing>;
  let flushes = 0;
  const flushFn = async () => {
    flushes++;
  };
  beforeEach(() => {
    t = setupTracing(1, 'orders');
    resetColdStart();
    flushes = 0;
    process.env.AWS_LAMBDA_FUNCTION_NAME = 'orders-api';
    process.env.AWS_REGION = 'eu-central-1';
  });
  afterEach(async () => {
    delete process.env.AWS_LAMBDA_FUNCTION_NAME;
    delete process.env.AWS_REGION;
    await t.teardown();
  });

  it('async http handler: entry span, cold start only once, flush before return, parent from traceparent', async () => {
    const tracer = trace.getTracer('caller');
    const caller = tracer.startSpan('caller', { kind: SpanKind.CLIENT });
    const headers = context.with(trace.setSpan(ROOT_CONTEXT, caller), () => injectHeaders({} as Record<string, string>));
    caller.end();
    const h = wrapLambda(async (event: typeof apiV2) => {
      expect(trace.getActiveSpan()).toBeDefined();
      return { statusCode: 200, body: event.rawPath };
    }, { flushFn });
    const ev = { ...apiV2, headers: { Traceparent: headers.traceparent! } };
    expect(await h(ev, lctx)).toEqual({ statusCode: 200, body: '/users/42' });
    expect(flushes).toBe(1);
    await h(apiV2, { ...lctx, awsRequestId: 'req-2' });
    const spans = t.spans().filter((s) => s.attributes['faas.name'] === 'orders-api');
    expect(spans).toHaveLength(2);
    const [first, second] = spans;
    expect(first!.name).toBe('GET /users/:id');
    expect(first!.kind).toBe(SpanKind.SERVER);
    expect(first!.attributes).toMatchObject({
      'faas.trigger': 'http',
      'faas.coldstart': true,
      'faas.invocation_id': 'req-1',
      'cloud.provider': 'aws',
      'cloud.region': 'eu-central-1',
      'http.request.method': 'GET',
      'http.route': '/users/:id',
      'http.response.status_code': 200,
    });
    expect(first!.spanContext().traceId).toBe(caller.spanContext().traceId);
    expect(second!.attributes['faas.coldstart']).toBe(false);
    expect(second!.attributes['faas.invocation_id']).toBe('req-2');
  });

  it('REST API resource template and 5xx results', async () => {
    const h = wrapLambda(async () => ({ statusCode: 502 }), { flushFn });
    await h(apiV1, lctx);
    const s = t.span('POST /orders/:orderId');
    expect(s.status.code).toBe(SpanStatusCode.ERROR);
  });

  it('callback handler + sqs batch → CONSUMER span', async () => {
    const h = wrapLambda((_event: typeof sqs, _ctx, cb) => {
      setTimeout(() => cb(null, { batchItemFailures: [] }), 1);
    }, { flushFn });
    expect(await h(sqs, lctx)).toEqual({ batchItemFailures: [] });
    const s = t.span('process orders');
    expect(s.kind).toBe(SpanKind.CONSUMER);
    expect(s.attributes).toMatchObject({
      'faas.trigger': 'pubsub',
      'messaging.system': 'aws_sqs',
      'messaging.destination.name': 'orders',
      'messaging.batch.message_count': 2,
      'trooper.process': 'orders-api',
    });
    expect(flushes).toBe(1);
  });

  it('handler errors are recorded, flushed and re-thrown unchanged (async and callback)', async () => {
    const boom = new Error('boom');
    const a = wrapLambda(async () => {
      throw boom;
    }, { flushFn });
    await expect(a({ source: 'aws.events', 'detail-type': 'Scheduled Event' }, lctx)).rejects.toBe(boom);
    const s = t.span('orders-api');
    expect(s.attributes['faas.trigger']).toBe('timer');
    expect(s.status.code).toBe(SpanStatusCode.ERROR);
    expect(s.events.some((e) => e.name === 'exception')).toBe(true);
    const c = wrapLambda((_e: unknown, _c, cb) => cb(boom), { flushFn });
    await expect(c({}, lctx)).rejects.toBe(boom);
    const sync = wrapLambda((): number => {
      throw boom;
    }, { flushFn });
    await expect(sync({}, lctx)).rejects.toBe(boom);
    expect(flushes).toBe(3);
  });

  it('telemetry failures never surface: failing / hanging flush, odd inputs', async () => {
    const before = internalErrorCount();
    const failing = wrapLambda(async () => 'ok', {
      flushFn: async () => {
        throw new Error('ingest down');
      },
    });
    expect(await failing(undefined as unknown as object, undefined as unknown as typeof lctx)).toBe('ok');
    expect(internalErrorCount()).toBeGreaterThan(before);
    const hanging = wrapLambda(async () => 'ok', { flushFn: () => new Promise<void>(() => {}), flushTimeoutMs: 20 });
    const started = Date.now();
    expect(await hanging({}, lctx)).toBe('ok');
    expect(Date.now() - started).toBeLessThan(1000);
    // Without init(): getHandle() is undefined → no-op flush.
    expect(await wrapLambda(async () => 1)({}, lctx)).toBe(1);
  });

  it('name/trigger overrides and context fallback', async () => {
    delete process.env.AWS_LAMBDA_FUNCTION_NAME;
    await wrapLambda(async () => null, { flushFn })({}, lctx);
    expect(t.span('ctx-name').attributes['faas.trigger']).toBe('other');
    await wrapLambda(async () => null, { flushFn, name: 'custom', trigger: 'timer' })({}, lctx);
    expect(t.span('custom').attributes['faas.trigger']).toBe('timer');
  });
});

