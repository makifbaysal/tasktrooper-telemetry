import { logs, metrics, propagation, trace } from '@opentelemetry/api';
import { ExportResultCode, type ExportResult } from '@opentelemetry/core';
import { OTLPLogExporter } from '@opentelemetry/exporter-logs-otlp-proto';
import { OTLPMetricExporter } from '@opentelemetry/exporter-metrics-otlp-proto';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-proto';
import type { Instrumentation } from '@opentelemetry/instrumentation';
import { AmqplibInstrumentation } from '@opentelemetry/instrumentation-amqplib';
import { ExpressInstrumentation } from '@opentelemetry/instrumentation-express';
import { HttpInstrumentation } from '@opentelemetry/instrumentation-http';
import { IORedisInstrumentation } from '@opentelemetry/instrumentation-ioredis';
import { KafkaJsInstrumentation } from '@opentelemetry/instrumentation-kafkajs';
import { KoaInstrumentation } from '@opentelemetry/instrumentation-koa';
import { MongoDBInstrumentation } from '@opentelemetry/instrumentation-mongodb';
import { MySQL2Instrumentation } from '@opentelemetry/instrumentation-mysql2';
import { NestInstrumentation } from '@opentelemetry/instrumentation-nestjs-core';
import { PgInstrumentation } from '@opentelemetry/instrumentation-pg';
import { PinoInstrumentation } from '@opentelemetry/instrumentation-pino';
import { RedisInstrumentation } from '@opentelemetry/instrumentation-redis';
import { UndiciInstrumentation } from '@opentelemetry/instrumentation-undici';
import { WinstonInstrumentation } from '@opentelemetry/instrumentation-winston';
import { defaultResource, resourceFromAttributes } from '@opentelemetry/resources';
import { BatchLogRecordProcessor, type LogRecordProcessor } from '@opentelemetry/sdk-logs';
import { PeriodicExportingMetricReader, type IMetricReader } from '@opentelemetry/sdk-metrics';
import { NodeSDK } from '@opentelemetry/sdk-node';
import { BatchSpanProcessor, type SpanProcessor } from '@opentelemetry/sdk-trace';
import { DISTRO_NAME, SELF_METER, VERSION } from './attrs';
import { resolveConfig, type ResolvedConfig, type TrooperOptions } from './config';
import { bindSelfMetrics, recordError, setDebug } from './errors';
import { amqplibConsumeHook, kafkajsConsumerHook } from './messaging';
import { TrooperSpanProcessor } from './processor';
import { ContinuousProfiler, ProfileUploader } from './profiling';
import { createPropagator } from './propagation';
import { TrooperSampler } from './sampler';

export interface TrooperHandle {
  /** Resolved configuration (no secrets beyond the ingest key header). */
  readonly config: ResolvedConfig;
  /** Whether telemetry is exported (a DSN or OTLP endpoint was configured). */
  readonly exporting: boolean;
  /** Flushes pending spans / metrics / logs. Never rejects. */
  forceFlush(): Promise<void>;
  /** Flushes and shuts down. Never rejects. */
  shutdown(): Promise<void>;
}

let current: TrooperHandle | undefined;

type Flushable = { forceFlush?: () => Promise<unknown>; getDelegate?: () => unknown; _getDelegate?: () => unknown };

/** Flushes a global provider, unwrapping the API's proxy providers. */
async function flush(provider: unknown): Promise<void> {
  let p = provider as Flushable | undefined;
  if (p && typeof p.forceFlush !== 'function') {
    if (typeof p.getDelegate === 'function') p = p.getDelegate() as Flushable;
    else if (typeof p._getDelegate === 'function') p = p._getDelegate() as Flushable; // api-logs proxy
  }
  if (p && typeof p.forceFlush === 'function') await p.forceFlush();
}

/** Wraps an OTLP exporter so failed batches are counted in `trooper.sdk.errors{component=exporter.<signal>}`. */
function counting<T extends object>(exporter: T, signal: string): T {
  return new Proxy(exporter, {
    get(target, prop, receiver) {
      const v = Reflect.get(target, prop, receiver);
      if (prop !== 'export' || typeof v !== 'function') return typeof v === 'function' ? v.bind(target) : v;
      return (items: unknown, cb: (r: ExportResult) => void) => {
        try {
          (v as (i: unknown, c: (r: ExportResult) => void) => void).call(target, items, (r: ExportResult) => {
            if (r.code !== ExportResultCode.SUCCESS) recordError('exporter.' + signal, r.error);
            cb(r);
          });
        } catch (err) {
          recordError('exporter.' + signal, err);
          cb({ code: ExportResultCode.FAILED, error: err instanceof Error ? err : new Error(String(err)) });
        }
      };
    },
  });
}

function defaultInstrumentations(cfg: ResolvedConfig): Instrumentation[] {
  const off = cfg.disableInstrumentations;
  const factories: [string, () => Instrumentation][] = [
    ['http', () => new HttpInstrumentation()],
    ['undici', () => new UndiciInstrumentation()],
    ['express', () => new ExpressInstrumentation()],
    ['koa', () => new KoaInstrumentation()],
    ['nestjs', () => new NestInstrumentation()],
    ['kafkajs', () => new KafkaJsInstrumentation({ consumerHook: kafkajsConsumerHook })],
    ['amqplib', () => new AmqplibInstrumentation({ consumeHook: amqplibConsumeHook })],
    ['pino', () => new PinoInstrumentation({ disableLogSending: !cfg.logsUrl })],
    ['winston', () => new WinstonInstrumentation({ disableLogSending: !cfg.logsUrl })],
    ['pg', () => new PgInstrumentation()],
    ['mysql2', () => new MySQL2Instrumentation()],
    ['mongodb', () => new MongoDBInstrumentation()],
    ['ioredis', () => new IORedisInstrumentation()],
    ['redis', () => new RedisInstrumentation()],
  ];
  const out: Instrumentation[] = [];
  if (off === true) return out;
  for (const [name, make] of factories) {
    if (off.has(name)) continue;
    try {
      out.push(make());
    } catch (err) {
      recordError('instrumentation.' + name, err);
    }
  }
  return out;
}

function noopHandle(config: ResolvedConfig): TrooperHandle {
  return { config, exporting: false, forceFlush: async () => {}, shutdown: async () => {} };
}

/**
 * Starts the TaskTrooper distribution of OpenTelemetry: OTLP/HTTP protobuf export of traces, metrics and logs to the
 * DSN's ingest gateway, trooper sampler / propagator / span processor and the default instrumentations.
 * Call it before importing the frameworks it instruments (or use `node --import @trooper/node/register`).
 * Idempotent; never throws — on failure it returns a handle that does nothing.
 */
export function init(options: TrooperOptions = {}): TrooperHandle {
  if (current) return current;
  let cfg: ResolvedConfig;
  try {
    cfg = resolveConfig(options);
  } catch (err) {
    recordError('init', err);
    cfg = resolveConfig({ enabled: false }, {});
  }
  setDebug(cfg.debug);
  if (cfg.dsnError) recordError('dsn', new Error(cfg.dsnError));
  if (!cfg.enabled) {
    current = noopHandle(cfg);
    return current;
  }
  try {
    const exportOpts = (url: string) => ({ url, headers: cfg.headers, timeoutMillis: cfg.exportTimeoutMs });
    const spanProcessors: SpanProcessor[] = [new TrooperSpanProcessor(cfg.sampleRate)];
    if (cfg.tracesUrl) {
      spanProcessors.push(
        new BatchSpanProcessor({
          exporter: counting(new OTLPTraceExporter(exportOpts(cfg.tracesUrl)), 'traces'),
          scheduledDelayMillis: cfg.batchDelayMs,
          maxQueueSize: cfg.maxQueueSize,
          exportTimeoutMillis: cfg.exportTimeoutMs,
        }),
      );
    }
    const metricReaders: IMetricReader[] = [];
    if (cfg.metricsUrl) {
      metricReaders.push(
        new PeriodicExportingMetricReader({
          exporter: counting(new OTLPMetricExporter(exportOpts(cfg.metricsUrl)), 'metrics'),
          exportIntervalMillis: cfg.metricIntervalMs,
          exportTimeoutMillis: Math.min(cfg.exportTimeoutMs, cfg.metricIntervalMs),
        }),
      );
    }
    const logRecordProcessors: LogRecordProcessor[] = [];
    if (cfg.logsUrl) {
      logRecordProcessors.push(
        new BatchLogRecordProcessor({
          exporter: counting(new OTLPLogExporter(exportOpts(cfg.logsUrl)), 'logs'),
          scheduledDelayMillis: cfg.batchDelayMs,
          maxQueueSize: cfg.maxQueueSize,
          exportTimeoutMillis: cfg.exportTimeoutMs,
        }),
      );
    }
    const resAttrs: Record<string, string | number | boolean> = {
      ...cfg.resourceAttributes,
      'service.name': cfg.service,
      'telemetry.distro.name': DISTRO_NAME,
      'telemetry.distro.version': VERSION,
    };
    if (cfg.version) resAttrs['service.version'] = cfg.version;
    if (cfg.environment) resAttrs['deployment.environment.name'] = cfg.environment;

    const sdk = new NodeSDK({
      resource: defaultResource().merge(resourceFromAttributes(resAttrs)),
      serviceName: cfg.service,
      sampler: new TrooperSampler(cfg.sampleRate),
      spanProcessors,
      metricReaders,
      logRecordProcessors,
      textMapPropagator: createPropagator(cfg.service),
      instrumentations: [...defaultInstrumentations(cfg), ...cfg.instrumentations],
    });
    sdk.start();
    bindSelfMetrics(metrics.getMeter(SELF_METER, VERSION));

    let profiler: ContinuousProfiler | undefined;
    if (cfg.profiling && cfg.profilesUrl) {
      try {
        profiler = new ContinuousProfiler({
          config: cfg.profiling,
          uploader: new ProfileUploader(cfg.profilesUrl, cfg.headers, cfg.exportTimeoutMs),
          service: cfg.service,
          env: cfg.environment,
          version: cfg.version,
        });
        void profiler.start();
      } catch (err) {
        recordError('profiler', err);
      }
    }

    const exporting = !!(cfg.tracesUrl || cfg.metricsUrl || cfg.logsUrl);
    let stopped = false;
    const handle: TrooperHandle = {
      config: cfg,
      exporting,
      async forceFlush() {
        try {
          await Promise.all([flush(trace.getTracerProvider()), flush(metrics.getMeterProvider()), flush(logs.getLoggerProvider())]);
        } catch (err) {
          recordError('flush', err);
        }
      },
      async shutdown() {
        if (stopped) return;
        stopped = true;
        try {
          if (profiler) await profiler.stop(cfg.exportTimeoutMs);
          await sdk.shutdown();
        } catch (err) {
          recordError('shutdown', err);
        }
        if (current === handle) current = undefined;
      },
    };
    // Flush on natural process exit (no signal handlers: the host owns SIGTERM handling).
    process.once('beforeExit', () => {
      void handle.shutdown();
    });
    current = handle;
    return handle;
  } catch (err) {
    recordError('init', err);
    try {
      // leave the process in a clean, propagation-only state
      propagation.setGlobalPropagator(createPropagator(cfg.service));
    } catch {
      /* ignore */
    }
    current = noopHandle(cfg);
    return current;
  }
}

/** The handle of the running SDK, if `init` was called. */
export function getHandle(): TrooperHandle | undefined {
  return current;
}
