import { context, propagation, trace } from '@opentelemetry/api';
import { AsyncLocalStorageContextManager } from '@opentelemetry/context-async-hooks';
import { InMemorySpanExporter, SamplingDecision, SimpleSpanProcessor, TracerProvider, type Sampler } from '@opentelemetry/sdk-trace';
import { createPropagator } from '../src/propagation';
import { TrooperSampler } from '../src/sampler';
import { TrooperSpanProcessor } from '../src/processor';

/** TrooperSampler that records every span (deterministic tests) but still writes tracestate. */
function alwaysOn(rate: number): Sampler {
  const inner = new TrooperSampler(rate);
  return {
    shouldSample: (...args: Parameters<Sampler['shouldSample']>) => ({ ...inner.shouldSample(...args), decision: SamplingDecision.RECORD_AND_SAMPLED }),
    toString: () => 'alwaysOn',
  };
}

/** Global tracer provider with the trooper sampler/processor/propagator and an in-memory exporter. */
export function setupTracing(rate = 1, service = 'svc-a') {
  const exporter = new InMemorySpanExporter();
  const provider = new TracerProvider({
    sampler: alwaysOn(rate),
    spanProcessors: [new TrooperSpanProcessor(rate), new SimpleSpanProcessor({ exporter })],
  });
  const cm = new AsyncLocalStorageContextManager().enable();
  context.setGlobalContextManager(cm);
  trace.setGlobalTracerProvider(provider);
  propagation.setGlobalPropagator(createPropagator(service));
  return {
    exporter,
    provider,
    tracer: trace.getTracer('test'),
    spans: () => exporter.getFinishedSpans(),
    span: (name: string) => {
      const s = exporter.getFinishedSpans().find((x) => x.name === name);
      if (!s) throw new Error(`span ${name} not found`);
      return s;
    },
    teardown: async () => {
      await provider.shutdown();
      trace.disable();
      context.disable();
      propagation.disable();
    },
  };
}
