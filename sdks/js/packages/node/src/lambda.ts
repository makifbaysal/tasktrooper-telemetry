import {
  context as otelContext,
  propagation,
  ROOT_CONTEXT,
  SpanKind,
  SpanStatusCode,
  trace,
  type Attributes,
  type Context,
  type Span,
} from '@opentelemetry/api';
import { ATTR_PROCESS, SCOPE_NAME, VERSION } from './attrs';
import { recordError } from './errors';
import { getHandle } from './init';
import { headersGetter } from './messaging';
import { normalizeRoute } from './route';

/** OTel semconv `faas.trigger` values. */
export type FaasTrigger = 'http' | 'pubsub' | 'datasource' | 'timer' | 'other';

/** The subset of the AWS Lambda context object the wrapper reads. */
export interface LambdaContext {
  awsRequestId?: string;
  functionName?: string;
  functionVersion?: string;
  invokedFunctionArn?: string;
  memoryLimitInMB?: string | number;
  [key: string]: unknown;
}

export type LambdaCallback<R = unknown> = (error?: unknown, result?: R) => void;
export type AsyncLambdaHandler<E = any, R = any> = (event: E, context: LambdaContext) => Promise<R> | R;
export type CallbackLambdaHandler<E = any, R = any> = (event: E, context: LambdaContext, callback: LambdaCallback<R>) => void;

export interface WrapLambdaOptions {
  /** `faas.name`; default `AWS_LAMBDA_FUNCTION_NAME`, then `context.functionName`. */
  name?: string;
  /** `faas.trigger`; inferred from the event shape when omitted. */
  trigger?: FaasTrigger;
  /** Flush before returning (default true). Lambda freezes the process after the handler returns. */
  flush?: boolean;
  /** Upper bound for the flush in ms (default 2000) — a slow ingest never holds the invocation longer. */
  flushTimeoutMs?: number;
  /** Continue the caller's trace from HTTP headers (`traceparent`, `baggage`) when present (default true). */
  propagate?: boolean;
  /** Custom flush (tests / custom providers); default `getHandle()?.forceFlush()`. */
  flushFn?: () => Promise<void>;
}

const env = (k: string): string | undefined => {
  try {
    const v = process.env[k];
    return v ? v : undefined;
  } catch {
    return undefined;
  }
};

let coldStart = true;

/** Test hook: the next invocation is reported as a cold start again. */
export function resetColdStart(): void {
  coldStart = true;
}

type AnyEvent = Record<string, any> | null | undefined;

function firstRecord(e: AnyEvent): Record<string, any> | undefined {
  const r = e && Array.isArray(e.Records) ? e.Records[0] : undefined;
  return r && typeof r === 'object' ? r : undefined;
}

/**
 * Infers `faas.trigger` from the event shape: API Gateway (REST / HTTP API), Function URL and ALB → `http`;
 * SQS / SNS / Kinesis / EventBridge events / Kafka / MQ → `pubsub`; DynamoDB streams and S3 → `datasource`;
 * EventBridge scheduled events → `timer`; anything else → `other`.
 */
export function inferTrigger(event: unknown): FaasTrigger {
  try {
    const e = event as AnyEvent;
    if (!e || typeof e !== 'object') return 'other';
    if (e.requestContext && (e.httpMethod || e.requestContext.http || e.requestContext.elb || e.routeKey)) return 'http';
    if (typeof e.httpMethod === 'string' && e.headers) return 'http';
    const r = firstRecord(e);
    if (r) {
      const src = String(r.eventSource ?? r.EventSource ?? '');
      if (src === 'aws:dynamodb' || src === 'aws:s3') return 'datasource';
      if (src === 'aws:sqs' || src === 'aws:sns' || src === 'aws:kinesis' || src === 'aws:kafka' || src.startsWith('aws:')) return 'pubsub';
      if (r.s3) return 'datasource';
      if (r.Sns) return 'pubsub';
    }
    if (e.eventSource === 'aws:kafka' || e.eventSource === 'SelfManagedKafka' || e.eventSource === 'aws:mq' || e.eventSource === 'aws:rmq') return 'pubsub';
    if (e.source === 'aws.events' || e['detail-type'] === 'Scheduled Event' || e.source === 'aws.scheduler') return 'timer';
    if (e['detail-type'] !== undefined && e.source !== undefined) return 'pubsub'; // EventBridge event
  } catch (err) {
    recordError('lambda', err);
  }
  return 'other';
}

/** HTTP method and route template of an API Gateway / Function URL / ALB event. */
function httpInfo(e: Record<string, any>): { method?: string; route?: string } {
  const rc = e.requestContext ?? {};
  const method = String(e.httpMethod ?? rc.http?.method ?? '').toUpperCase() || undefined;
  let route: string | undefined;
  if (typeof e.routeKey === 'string' && e.routeKey !== '$default') {
    // HTTP API: "GET /users/{id}"
    const parts = e.routeKey.split(' ');
    route = parts.length > 1 ? parts.slice(1).join(' ') : e.routeKey;
  } else if (typeof e.resource === 'string' && e.resource !== '') {
    route = e.resource; // REST API: "/users/{id}" or "/{proxy+}"
  }
  if (route && /\{[^}]*\+\}/.test(route)) route = undefined; // greedy proxy: no useful template
  if (!route) {
    const raw = e.rawPath ?? e.path ?? rc.http?.path;
    if (typeof raw === 'string') route = raw.split('?')[0]; // normalized (ids → :id) below
  }
  return { method, route: normalizeRoute(route) };
}

function queueOf(e: AnyEvent): { system?: string; destination?: string } {
  const r = firstRecord(e);
  if (!r) return {};
  const arn = String(r.eventSourceARN ?? r.EventSubscriptionArn ?? r.TopicArn ?? r.Sns?.TopicArn ?? '');
  const src = String(r.eventSource ?? r.EventSource ?? '');
  const system = src === 'aws:sqs' ? 'aws_sqs' : src === 'aws:sns' ? 'aws_sns' : src === 'aws:kinesis' ? 'aws_kinesis' : undefined;
  if (!arn) return { system };
  const last = arn.split(':').pop() ?? '';
  return { system, destination: (last.includes('/') ? last.split('/')[1] : last) || undefined };
}

interface Invocation {
  span?: Span;
  ctx: Context;
}

function startSpan(event: unknown, lctx: LambdaContext, opts: WrapLambdaOptions): Invocation {
  let ctx = otelContext.active();
  try {
    const e = (event && typeof event === 'object' ? event : {}) as Record<string, any>;
    const name = opts.name ?? env('AWS_LAMBDA_FUNCTION_NAME') ?? lctx?.functionName ?? 'lambda';
    const trigger = opts.trigger ?? inferTrigger(event);
    const attrs: Attributes = {
      'faas.name': name,
      'faas.trigger': trigger,
      'faas.coldstart': coldStart,
      'cloud.provider': 'aws',
      'cloud.platform': 'aws_lambda',
    };
    coldStart = false;
    if (lctx?.awsRequestId) attrs['faas.invocation_id'] = String(lctx.awsRequestId);
    const version = env('AWS_LAMBDA_FUNCTION_VERSION') ?? lctx?.functionVersion;
    if (version) attrs['faas.version'] = version;
    const region = env('AWS_REGION');
    if (region) attrs['cloud.region'] = region;
    const mem = Number(env('AWS_LAMBDA_FUNCTION_MEMORY_SIZE') ?? lctx?.memoryLimitInMB);
    if (Number.isFinite(mem) && mem > 0) attrs['faas.max_memory'] = mem * 1024 * 1024;
    if (lctx?.invokedFunctionArn) attrs['cloud.resource_id'] = String(lctx.invokedFunctionArn);

    let parent = ROOT_CONTEXT;
    let kind = SpanKind.SERVER;
    let spanName = name;
    if (trigger === 'http') {
      const { method, route } = httpInfo(e);
      if (method) attrs['http.request.method'] = method;
      if (route) attrs['http.route'] = route;
      spanName = [method, route].filter(Boolean).join(' ') || name;
      if (opts.propagate !== false && e.headers && typeof e.headers === 'object') {
        parent = propagation.extract(ROOT_CONTEXT, lowerKeys(e.headers), headersGetter);
      }
    } else if (trigger === 'pubsub') {
      kind = SpanKind.CONSUMER;
      const q = queueOf(e);
      attrs['messaging.operation.type'] = 'process';
      if (q.system) attrs['messaging.system'] = q.system;
      if (q.destination) attrs['messaging.destination.name'] = q.destination;
      attrs[ATTR_PROCESS] = name;
      if (Array.isArray(e.Records)) attrs['messaging.batch.message_count'] = e.Records.length;
      spanName = `process ${q.destination ?? name}`;
    }
    const span = trace.getTracer(SCOPE_NAME, VERSION).startSpan(spanName, { kind, attributes: attrs }, parent);
    ctx = trace.setSpan(parent, span);
    return { span, ctx };
  } catch (err) {
    recordError('lambda', err);
    return { ctx };
  }
}

function lowerKeys(h: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(h)) out[k.toLowerCase()] = v;
  return out;
}

function endSpan(span: Span | undefined, err: unknown, failed: boolean, result?: unknown): void {
  if (!span) return;
  try {
    if (failed) {
      span.recordException(err instanceof Error ? err : new Error(String(err)));
      span.setStatus({ code: SpanStatusCode.ERROR, message: err instanceof Error ? err.message : String(err) });
    } else if (result && typeof result === 'object' && typeof (result as { statusCode?: unknown }).statusCode === 'number') {
      const code = (result as { statusCode: number }).statusCode;
      span.setAttribute('http.response.status_code', code);
      if (code >= 500) span.setStatus({ code: SpanStatusCode.ERROR });
    }
    span.end();
  } catch (e) {
    recordError('lambda', e);
  }
}

async function flush(opts: WrapLambdaOptions): Promise<void> {
  if (opts.flush === false) return;
  try {
    const fn = opts.flushFn ?? (() => getHandle()?.forceFlush() ?? Promise.resolve());
    const timeout = opts.flushTimeoutMs ?? 2000;
    let timer: ReturnType<typeof setTimeout> | undefined;
    await Promise.race([
      Promise.resolve()
        .then(fn)
        .catch((err: unknown) => recordError('lambda.flush', err)),
      new Promise<void>((resolve) => {
        timer = setTimeout(resolve, timeout);
        timer.unref?.();
      }),
    ]);
    if (timer) clearTimeout(timer);
  } catch (err) {
    recordError('lambda.flush', err);
  }
}

/**
 * Wraps an AWS Lambda handler (async or callback style) in an entry span with the OTel FaaS conventions:
 * `faas.name`, `faas.trigger`, `faas.coldstart` (true on the first invocation of the execution environment),
 * `faas.invocation_id` (awsRequestId), `cloud.provider=aws`. HTTP events (API Gateway, Function URL, ALB) get a SERVER
 * span named `<METHOD> <route>` that continues the caller's `traceparent`; SQS / SNS / Kinesis batches a CONSUMER span.
 * Telemetry is flushed before the wrapper resolves. Handler errors are recorded and re-thrown unchanged; telemetry
 * errors never surface. Call `init()` once at module scope before wrapping.
 *
 * ```ts
 * init({ service: 'orders' });
 * export const handler = wrapLambda(async (event) => ({ statusCode: 200, body: 'ok' }));
 * ```
 */
export function wrapLambda<E = any, R = any>(
  handler: AsyncLambdaHandler<E, R> | CallbackLambdaHandler<E, R>,
  opts: WrapLambdaOptions = {},
): (event: E, context: LambdaContext) => Promise<R> {
  const callbackStyle = typeof handler === 'function' && handler.length >= 3;
  return async function trooperLambda(this: unknown, event: E, lctx: LambdaContext): Promise<R> {
    const inv = startSpan(event, lctx, opts);
    let result: R;
    try {
      result = await otelContext.with(inv.ctx, () => {
        if (!callbackStyle) return (handler as AsyncLambdaHandler<E, R>).call(this, event, lctx);
        return new Promise<R>((resolve, reject) => {
          try {
            const r = (handler as CallbackLambdaHandler<E, R>).call(this, event, lctx, (err?: unknown, res?: R) =>
              err !== undefined && err !== null ? reject(err) : resolve(res as R),
            ) as unknown;
            // A callback-arity handler that returns a promise anyway.
            if (r && typeof (r as Promise<R>).then === 'function') (r as Promise<R>).then(resolve, reject);
          } catch (err) {
            reject(err);
          }
        });
      });
    } catch (err) {
      endSpan(inv.span, err, true);
      await flush(opts);
      throw err;
    }
    endSpan(inv.span, undefined, false, result);
    await flush(opts);
    return result;
  };
}
