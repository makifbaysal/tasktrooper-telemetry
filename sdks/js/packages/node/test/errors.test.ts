import { context, metrics, ROOT_CONTEXT, SpanKind, trace } from '@opentelemetry/api';
import { AggregationTemporality, InMemoryMetricExporter, MeterProvider, PeriodicExportingMetricReader } from '@opentelemetry/sdk-metrics';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { captureError, markDlq, setRoute, setRouteResolver, setSpanAttempt } from '../src/api';
import { attemptFromHeaders } from '../src/attempt';
import { bindSelfMetrics, internalErrorCount, recordError, resetSelfMetrics } from '../src/errors';
import { trooperExpress, trooperFastify, TrooperInterceptor } from '../src/frameworks';
import { amqplibConsumeHook, injectHeaders, instrumentBullmqProcessor, kafkajsConsumerHook, processMessage } from '../src/messaging';
import { TrooperSpanProcessor } from '../src/processor';
import { setupTracing } from './helpers';

let t: ReturnType<typeof setupTracing>;
beforeEach(() => {
  resetSelfMetrics();
  t = setupTracing();
});
afterEach(async () => {
  await t.teardown();
  metrics.disable();
});

describe('never throws into the host', () => {
  it('a throwing route resolver is swallowed and counted', () => {
    const span = t.tracer.startSpan('GET', { kind: SpanKind.SERVER });
    context.with(trace.setSpan(ROOT_CONTEXT, span), () => {
      setRouteResolver(() => {
        throw new Error('resolver bug');
      });
      t.tracer.startSpan('child').end();
    });
    span.end();
    expect(t.span('GET').attributes['trooper.route']).toBeUndefined();
    expect(internalErrorCount()).toBeGreaterThanOrEqual(1);
  });

  it('helpers tolerate missing spans and garbage input', () => {
    expect(() => {
      setRoute('/x');
      markDlq();
      captureError(new Error('no span'));
      captureError(null);
      setSpanAttempt(undefined, 2);
      kafkajsConsumerHook(undefined as never, undefined as never);
      amqplibConsumeHook(trace.wrapSpanContext({ traceId: 'x', spanId: 'y', traceFlags: 0 }), { msg: null as never });
      attemptFromHeaders({ 'x-death': 'nope', get 'tt-attempt'() {
        throw new Error('getter');
      } });
      injectHeaders(Object.freeze({}) as Record<string, unknown>);
      trooperExpress()({}, {}, () => {});
      trooperFastify({ addHook: () => { throw new Error('x'); } }, {}, () => {});
      new TrooperInterceptor().intercept({ switchToHttp: () => { throw new Error('nest'); } }, { handle: () => 1 });
    }).not.toThrow();
  });

  it('processor survives spans with odd attributes', () => {
    const p = new TrooperSpanProcessor(5);
    expect(p.sampleRate).toBe(1);
    expect(() => p.onStart(null as never, ROOT_CONTEXT)).not.toThrow();
    expect(() => p.onEnding(null as never)).not.toThrow();
  });

  it('user errors still propagate; bullmq wrapper rejects with the original error', async () => {
    const err = new Error('handler failed');
    await expect(instrumentBullmqProcessor(async () => Promise.reject(err))({ data: null })).rejects.toBe(err);
    await expect(processMessage({ system: 'kafka' }, async () => { throw err; })).rejects.toBe(err);
  });
});

describe('trooper.sdk.errors', () => {
  it('counts swallowed errors per component on meter TaskTrooper.Telemetry', async () => {
    recordError('before_bind'); // buffered until a meter is bound
    const exporter = new InMemoryMetricExporter(AggregationTemporality.CUMULATIVE);
    const reader = new PeriodicExportingMetricReader({ exporter, exportIntervalMillis: 60_000 });
    const mp = new MeterProvider({ readers: [reader] });
    bindSelfMetrics(mp.getMeter('TaskTrooper.Telemetry'));
    recordError('span_processor');
    recordError('span_processor');
    await reader.forceFlush();
    const scope = exporter.getMetrics()[0]!.scopeMetrics.find((s) => s.scope.name === 'TaskTrooper.Telemetry')!;
    const metric = scope.metrics.find((m) => m.descriptor.name === 'trooper.sdk.errors')!;
    const byComponent = Object.fromEntries(metric.dataPoints.map((d) => [d.attributes.component, d.value]));
    expect(byComponent).toEqual({ before_bind: 1, span_processor: 2 });
    await mp.shutdown();
  });
});
