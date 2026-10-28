import {
  context as otelContext,
  propagation,
  ROOT_CONTEXT,
  SpanKind,
  SpanStatusCode,
  trace,
  type Attributes,
  type Span,
  type TextMapGetter,
  type TextMapSetter,
} from '@opentelemetry/api';
import { ATTEMPT_HEADER, ATTR_PROCESS, SCOPE_NAME, VERSION } from './attrs';
import { attemptFromHeaders, headerText, parseAttempt } from './attempt';
import { setSpanAttempt } from './api';
import { recordError } from './errors';

type Carrier = Record<string, unknown>;

/** Text map getter for message headers whose values may be Buffers / strings / arrays (kafkajs, amqplib). */
export const headersGetter: TextMapGetter<Carrier> = {
  keys: (c) => (c ? Object.keys(c) : []),
  get: (c, key) => {
    if (!c) return undefined;
    const v = c[key] ?? c[key.toLowerCase()];
    return headerText(v);
  },
};

export const headersSetter: TextMapSetter<Carrier> = {
  set: (c, key, value) => {
    if (c) c[key] = value;
  },
};

/** Injects the current trace context + baggage (with `tt.svc`) into message headers. Returns the headers. */
export function injectHeaders<T extends Carrier>(headers?: T): T {
  const h = (headers ?? {}) as T;
  try {
    propagation.inject(otelContext.active(), h, headersSetter);
  } catch (err) {
    recordError('propagator', err);
  }
  return h;
}

// ---- upstream instrumentation hooks ---------------------------------------------------------------------------

/** kafkajs `consumerHook`: `trooper.consumer.attempt` from `tt-attempt` / `kafka_deliveryAttempt` (default 1). */
export function kafkajsConsumerHook(span: Span, info: { topic: string; message: { headers?: Carrier } }): void {
  try {
    setSpanAttempt(span, attemptFromHeaders(info?.message?.headers) ?? 1);
  } catch (err) {
    recordError('kafkajs', err);
  }
}

interface AmqpMessage {
  fields?: { redelivered?: boolean };
  properties?: { headers?: Carrier };
}

/**
 * amqplib `consumeHook`: attempt from `tt-attempt` → `x-delivery-count` + 1 → `x-death` + 1, else 2 when the
 * broker flags a redelivery, else 1.
 */
export function amqplibConsumeHook(span: Span, info: { msg: AmqpMessage }): void {
  try {
    const msg = info?.msg;
    const a = attemptFromHeaders(msg?.properties?.headers) ?? (msg?.fields?.redelivered ? 2 : 1);
    setSpanAttempt(span, a);
  } catch (err) {
    recordError('amqplib', err);
  }
}

// ---- generic consumer ---------------------------------------------------------------------------------------------

export interface MessageInfo {
  /** messaging.system: `kafka`, `rabbitmq`, `bullmq` … */
  system: string;
  /** Topic / queue (messaging.destination.name). */
  destination?: string;
  /** messaging.consumer.group.name. */
  consumerGroup?: string;
  /** trooper.process; defaults to consumerGroup, then destination. */
  process?: string;
  /** Message headers carrying traceparent / baggage / tt-attempt. */
  headers?: Carrier;
  /** Attempt (1 = first delivery); a larger `tt-attempt` header wins. */
  attempt?: number;
  /** Extra span attributes (low cardinality). */
  attributes?: Attributes;
}

/**
 * Runs `fn` inside a consumer ("process <destination>") entry span whose parent is extracted from `headers`:
 * `trooper.process`, `trooper.consumer.attempt`, `trooper.caller` and `trooper.entry.route` are set by the span
 * processor. Errors thrown by `fn` are recorded and re-thrown unchanged; the SDK's own failures never surface.
 */
export function processMessage<T>(info: MessageInfo, fn: () => T): T {
  let span: Span | undefined;
  let ctx = otelContext.active();
  try {
    const parent = info.headers ? propagation.extract(ROOT_CONTEXT, info.headers, headersGetter) : ROOT_CONTEXT;
    const proc = info.process || info.consumerGroup || info.destination;
    const attrs: Attributes = { 'messaging.system': info.system, 'messaging.operation.type': 'process', ...info.attributes };
    if (info.destination) attrs['messaging.destination.name'] = info.destination;
    if (info.consumerGroup) attrs['messaging.consumer.group.name'] = info.consumerGroup;
    if (proc) attrs[ATTR_PROCESS] = proc;
    span = trace
      .getTracer(SCOPE_NAME, VERSION)
      .startSpan(`process ${info.destination ?? proc ?? info.system}`, { kind: SpanKind.CONSUMER, attributes: attrs }, parent);
    let attempt = info.attempt && info.attempt > 0 ? info.attempt : 1;
    const tt = parseAttempt(info.headers?.[ATTEMPT_HEADER]);
    if (tt !== undefined && tt > attempt) attempt = tt;
    setSpanAttempt(span, attempt);
    ctx = trace.setSpan(parent, span);
  } catch (err) {
    recordError('consumer', err);
  }
  const s = span;
  const end = (err?: unknown) => {
    if (!s) return;
    try {
      if (err !== undefined) {
        s.recordException(err instanceof Error ? err : new Error(String(err)));
        s.setStatus({ code: SpanStatusCode.ERROR, message: err instanceof Error ? err.message : String(err) });
      }
      s.end();
    } catch (e) {
      recordError('consumer', e);
    }
  };
  let result: T;
  try {
    result = otelContext.with(ctx, fn);
  } catch (err) {
    end(err);
    throw err;
  }
  if (result && typeof (result as { then?: unknown }).then === 'function') {
    return (result as unknown as Promise<unknown>).then(
      (v) => {
        end();
        return v;
      },
      (err: unknown) => {
        end(err);
        throw err;
      },
    ) as T;
  }
  end();
  return result;
}

// ---- BullMQ -------------------------------------------------------------------------------------------------------

/** Key in job data that carries the trace context (written by `withTraceContext`). */
export const BULLMQ_CARRIER_KEY = '_trooper';

interface BullJob {
  queueName?: string;
  name?: string;
  data?: unknown;
  attemptsMade?: number;
  attemptsStarted?: number;
}

/**
 * Producer side for BullMQ (jobs have no headers): returns a copy of `data` with the trace context and `tt-attempt`
 * (when given) under `_trooper`, so the worker continues the trace and sees `trooper.caller`.
 */
export function withTraceContext<T extends object>(data: T, attempt?: number): T & { [BULLMQ_CARRIER_KEY]: Record<string, string> } {
  const carrier: Record<string, string> = {};
  injectHeaders(carrier);
  if (attempt && attempt > 0) carrier[ATTEMPT_HEADER] = String(Math.floor(attempt));
  return { ...data, [BULLMQ_CARRIER_KEY]: carrier };
}

function bullAttempt(job: BullJob): number {
  if (typeof job.attemptsStarted === 'number' && job.attemptsStarted > 0) return job.attemptsStarted;
  return (typeof job.attemptsMade === 'number' && job.attemptsMade >= 0 ? job.attemptsMade : 0) + 1;
}

/**
 * Wraps a BullMQ `Worker` processor: every job runs in a consumer entry span (`messaging.system=bullmq`,
 * `trooper.process` = `process` option or the queue name, attempt from `tt-attempt` or BullMQ's own attempt count).
 *
 * ```ts
 * new Worker('emails', instrumentBullmqProcessor(async (job) => send(job.data)), { connection });
 * ```
 */
export function instrumentBullmqProcessor<J extends BullJob, R, A extends unknown[]>(
  processor: (job: J, ...rest: A) => Promise<R> | R,
  opts: { queueName?: string; process?: string } = {},
): (job: J, ...rest: A) => Promise<R> {
  return async (job: J, ...rest: A) => {
    let info: MessageInfo = { system: 'bullmq' };
    try {
      const data = job?.data as Record<string, unknown> | undefined;
      const carrier = data && typeof data === 'object' ? (data[BULLMQ_CARRIER_KEY] as Carrier | undefined) : undefined;
      const queue = opts.queueName ?? job?.queueName;
      info = {
        system: 'bullmq',
        destination: queue,
        process: opts.process ?? queue,
        headers: carrier && typeof carrier === 'object' ? carrier : undefined,
        attempt: bullAttempt(job ?? {}),
        attributes: job?.name ? { 'messaging.bullmq.job.name': job.name } : undefined,
      };
    } catch (err) {
      recordError('bullmq', err);
    }
    return processMessage(info, () => processor(job, ...rest));
  };
}
