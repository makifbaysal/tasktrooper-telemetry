import { createTraceState, trace, type Attributes, type Context, type Link, type SpanKind, type TraceState } from '@opentelemetry/api';
import { ParentBasedSampler, TraceIdRatioBasedSampler, type Sampler, type SamplingResult } from '@opentelemetry/sdk-trace';
import { TRACESTATE_KEY } from './attrs';
import { recordError } from './errors';

/**
 * ParentBased(TraceIdRatioBased(rate)) that writes the rate into the W3C tracestate of root spans as `tt=r:<rate>`
 * (.ai/contracts.md §4). Downstream entry spans read it so `trooper.sample_rate` reflects the rate that actually
 * decided the trace.
 */
export class TrooperSampler implements Sampler {
  readonly rate: number;
  private readonly inner: Sampler;

  constructor(rate = 1) {
    this.rate = rate > 0 && rate <= 1 ? rate : 1;
    this.inner = new ParentBasedSampler({ root: new TraceIdRatioBasedSampler(this.rate) });
  }

  shouldSample(context: Context, traceId: string, spanName: string, spanKind: SpanKind, attributes: Attributes, links: Link[]): SamplingResult {
    const res = this.inner.shouldSample(context, traceId, spanName, spanKind, attributes, links);
    try {
      const parent = trace.getSpanContext(context);
      if (!parent || !trace.isSpanContextValid(parent)) {
        const ts = (res.traceState ?? createTraceState()).set(TRACESTATE_KEY, 'r:' + formatRate(this.rate));
        return { ...res, traceState: ts };
      }
    } catch (err) {
      recordError('sampler', err);
    }
    return res;
  }

  toString(): string {
    return `TrooperSampler{rate=${this.rate}}`;
  }
}

export function formatRate(r: number): string {
  return String(r);
}

/** Parses `tt=r:<rate>` (sub-fields separated by `;`). */
export function rateFromTraceState(ts: TraceState | undefined): number | undefined {
  const v = ts?.get(TRACESTATE_KEY);
  if (!v) return undefined;
  for (const f of v.split(';')) {
    if (f.startsWith('r:')) {
      const r = Number(f.slice(2));
      if (Number.isFinite(r) && r > 0 && r <= 1) return r;
    }
  }
  return undefined;
}
