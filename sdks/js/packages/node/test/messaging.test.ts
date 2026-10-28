import { context, propagation, ROOT_CONTEXT, SpanKind, trace } from '@opentelemetry/api';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { currentAttempt, markDlq, nextAttempt, setProcessName } from '../src/api';
import { attemptFromHeaders, kafkaDeliveryAttempt, setAttemptHeader } from '../src/attempt';
import {
  amqplibConsumeHook,
  injectHeaders,
  instrumentBullmqProcessor,
  kafkajsConsumerHook,
  processMessage,
  withTraceContext,
} from '../src/messaging';
import { setupTracing } from './helpers';

describe('attempt headers', () => {
  it('order: tt-attempt → kafka_deliveryAttempt → x-delivery-count+1 → x-death+1', () => {
    expect(attemptFromHeaders({ 'tt-attempt': '4', kafka_deliveryAttempt: Buffer.from([0, 0, 0, 9]), 'x-delivery-count': 7 })).toBe(4);
    expect(attemptFromHeaders({ 'tt-attempt': Buffer.from('3') })).toBe(3);
    expect(attemptFromHeaders({ kafka_deliveryAttempt: Buffer.from([0, 0, 0, 2]) })).toBe(2);
    expect(attemptFromHeaders({ 'x-delivery-count': 2 })).toBe(3);
    expect(attemptFromHeaders({ 'x-delivery-count': '0' })).toBe(1);
    expect(attemptFromHeaders({ 'x-death': [{ count: 2 }, { count: 1 }] })).toBe(4);
    expect(attemptFromHeaders({ 'tt-attempt': 'garbage' })).toBeUndefined();
    expect(attemptFromHeaders({})).toBeUndefined();
    expect(attemptFromHeaders(undefined)).toBeUndefined();
  });

  it('kafka delivery attempt: binary or decimal', () => {
    expect(kafkaDeliveryAttempt(Buffer.from([0, 0, 1, 0]))).toBe(256);
    expect(kafkaDeliveryAttempt('5')).toBe(5);
    expect(kafkaDeliveryAttempt(Buffer.from('1234'))).toBe(1234);
  });

  it('setAttemptHeader writes a decimal string', () => {
    expect(setAttemptHeader({ a: 'b' }, 3)).toEqual({ a: 'b', 'tt-attempt': '3' });
    expect(setAttemptHeader(undefined, 0)).toEqual({ 'tt-attempt': '1' });
  });
});

describe('consumers', () => {
  let t: ReturnType<typeof setupTracing>;
  beforeEach(() => {
    t = setupTracing(1, 'producer-svc');
  });
  afterEach(async () => {
    await t.teardown();
  });

  it('processMessage: entry span with process, attempt, caller and entry route on children', async () => {
    const headers: Record<string, unknown> = {};
    const producer = t.tracer.startSpan('send', { kind: SpanKind.PRODUCER });
    context.with(trace.setSpan(ROOT_CONTEXT, producer), () => injectHeaders(headers));
    producer.end();
    headers['tt-attempt'] = Buffer.from('3');

    let next = 0;
    const out = await processMessage({ system: 'kafka', destination: 'orders', consumerGroup: 'billing', headers }, async () => {
      expect(currentAttempt()).toBe(3);
      next = nextAttempt();
      t.tracer.startSpan('db').end();
      return 42;
    });
    expect(out).toBe(42);
    expect(next).toBe(4);
    const s = t.span('process orders');
    expect(s.kind).toBe(SpanKind.CONSUMER);
    expect(s.parentSpanContext?.spanId).toBe(producer.spanContext().spanId);
    expect(s.attributes['trooper.process']).toBe('billing');
    expect(s.attributes['trooper.consumer.attempt']).toBe(3);
    expect(s.attributes['trooper.caller']).toBe('producer-svc');
    expect(s.attributes['trooper.entry.route']).toBe('billing');
    expect(s.attributes['trooper.entry.method']).toBe('CONSUME');
    expect(t.span('db').attributes['trooper.entry.route']).toBe('billing');
  });

  it('processMessage re-throws handler errors and records them', () => {
    expect(() =>
      processMessage({ system: 'rabbitmq', destination: 'q' }, () => {
        throw new Error('boom');
      }),
    ).toThrow('boom');
    const s = t.span('process q');
    expect(s.status.code).toBe(2);
    expect(s.events[0]?.name).toBe('exception');
  });

  it('setProcessName / markDlq', () => {
    processMessage({ system: 'kafka', destination: 'orders' }, () => {
      setProcessName('order-projector');
      markDlq();
    });
    const s = t.span('process orders');
    expect(s.attributes['trooper.process']).toBe('order-projector');
    expect(s.attributes['trooper.entry.route']).toBe('order-projector');
    expect(s.attributes['trooper.consumer.dlq']).toBe(true);
  });

  it('kafkajs / amqplib hooks set the attempt on upstream consumer spans', () => {
    const k = t.tracer.startSpan('orders process', { kind: SpanKind.CONSUMER, attributes: { 'messaging.destination.name': 'orders' } });
    kafkajsConsumerHook(k, { topic: 'orders', message: { headers: { 'tt-attempt': Buffer.from('2') } } });
    k.end();
    const r = t.tracer.startSpan('jobs process', { kind: SpanKind.CONSUMER, attributes: { 'messaging.destination': '' } });
    amqplibConsumeHook(r, { msg: { fields: { redelivered: true }, properties: { headers: {} } } });
    r.end();
    expect(t.span('orders process').attributes['trooper.consumer.attempt']).toBe(2);
    expect(t.span('orders process').attributes['trooper.process']).toBe('orders');
    expect(t.span('jobs process').attributes['trooper.consumer.attempt']).toBe(2);
    expect(t.span('jobs process').attributes['trooper.process']).toBe('jobs');
  });

  it('bullmq: trace context through job data, attempt from attemptsStarted', async () => {
    const parent = t.tracer.startSpan('enqueue');
    const data = context.with(trace.setSpan(ROOT_CONTEXT, parent), () => withTraceContext({ to: 'a@b.c' }));
    parent.end();
    const seen: unknown[] = [];
    const proc = instrumentBullmqProcessor(async (job: { data: unknown; queueName: string; attemptsStarted: number; name: string }) => {
      seen.push(job.data);
      return 'ok';
    });
    expect(await proc({ data, queueName: 'emails', attemptsStarted: 2, name: 'welcome' })).toBe('ok');
    const s = t.span('process emails');
    expect(s.attributes['messaging.system']).toBe('bullmq');
    expect(s.attributes['trooper.process']).toBe('emails');
    expect(s.attributes['trooper.consumer.attempt']).toBe(2);
    expect(s.attributes['trooper.caller']).toBe('producer-svc');
    expect(s.spanContext().traceId).toBe(parent.spanContext().traceId);
    expect(seen).toHaveLength(1);
  });

  it('propagation API sees tt.svc in injected headers', () => {
    const h = injectHeaders<Record<string, unknown>>();
    const ctx = propagation.extract(ROOT_CONTEXT, h, { keys: Object.keys, get: (c: Record<string, unknown>, k) => c[k] as string });
    expect(propagation.getBaggage(ctx)?.getEntry('tt.svc')?.value).toBe('producer-svc');
  });
});
