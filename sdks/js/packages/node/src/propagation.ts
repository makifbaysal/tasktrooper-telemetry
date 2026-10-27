import { propagation, type Context, type TextMapGetter, type TextMapPropagator, type TextMapSetter } from '@opentelemetry/api';
import { CompositePropagator, W3CBaggagePropagator, W3CTraceContextPropagator } from '@opentelemetry/core';
import { BAGGAGE_SERVICE_KEY } from './attrs';
import { recordError } from './errors';

/**
 * W3C baggage propagator that sets `tt.svc=<service>` on every injection (overriding the value received from an
 * upstream caller). The receiving trooper SDK records it as `trooper.caller`.
 */
export class TrooperBaggagePropagator implements TextMapPropagator {
  private readonly inner = new W3CBaggagePropagator();

  constructor(private readonly service: string | undefined) {}

  inject(context: Context, carrier: unknown, setter: TextMapSetter): void {
    let ctx = context;
    if (this.service) {
      try {
        const bag = propagation.getBaggage(context) ?? propagation.createBaggage();
        ctx = propagation.setBaggage(context, bag.setEntry(BAGGAGE_SERVICE_KEY, { value: this.service }));
      } catch (err) {
        recordError('propagator', err);
      }
    }
    this.inner.inject(ctx, carrier, setter);
  }

  extract(context: Context, carrier: unknown, getter: TextMapGetter): Context {
    return this.inner.extract(context, carrier, getter);
  }

  fields(): string[] {
    return this.inner.fields();
  }
}

/** W3C tracecontext + baggage with `tt.svc`. */
export function createPropagator(service: string | undefined): TextMapPropagator {
  return new CompositePropagator({
    propagators: [new W3CTraceContextPropagator(), new TrooperBaggagePropagator(service)],
  });
}

/** Calling service from W3C baggage member `tt.svc`. */
export function callerFromContext(ctx: Context): string | undefined {
  const v = propagation.getBaggage(ctx)?.getEntry(BAGGAGE_SERVICE_KEY)?.value;
  return v ? v : undefined;
}
