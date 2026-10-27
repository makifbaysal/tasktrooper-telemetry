import type { Instrumentation } from '@opentelemetry/instrumentation';

/** Parsed DSN: `https://<key>@<host>[:port][/<prefix>]/<project_id>` (.ai/contracts.md §2). */
export interface Dsn {
  /** Base URL of the ingest gateway without trailing slash; signal paths (`/v1/traces` …) are appended. */
  endpoint: string;
  /** Ingest key, sent as `x-trooper-key`. */
  key: string;
  /** Project id (informational; the key determines the tenant). */
  projectId?: string;
}

/** Parses a DSN. Throws on invalid input (callers swallow; `init` never throws). */
export function parseDsn(dsn: string): Dsn {
  const s = dsn.trim();
  if (!s) throw new Error('trooper: empty DSN');
  const u = new URL(s);
  if (u.protocol !== 'http:' && u.protocol !== 'https:') {
    throw new Error(`trooper: invalid DSN scheme ${u.protocol} (want http or https)`);
  }
  if (!u.hostname) throw new Error('trooper: DSN has no host');
  const key = decodeURIComponent(u.username);
  if (!key) throw new Error('trooper: DSN has no ingest key (https://<key>@host/<project>)');
  const path = u.pathname.replace(/^\/+|\/+$/g, '');
  let prefix = '';
  let projectId: string | undefined;
  if (path) {
    const i = path.lastIndexOf('/');
    if (i >= 0) {
      prefix = '/' + path.slice(0, i);
      projectId = path.slice(i + 1);
    } else projectId = path;
  }
  return { endpoint: `${u.protocol}//${u.host}${prefix}`, key, projectId };
}

export interface TrooperOptions {
  /** `https://<ingest-key>@<host>[:port]/<project>`. Env: `TROOPER_DSN`. Without DSN/endpoint nothing is exported. */
  dsn?: string;
  /** service.name. Env: `OTEL_SERVICE_NAME`, `TROOPER_SERVICE`; default `npm_package_name` or `unknown_service:node`. */
  service?: string;
  /** deployment.environment.name. Env: `TROOPER_ENV`. */
  environment?: string;
  /** service.version (deploy marker). Env: `TROOPER_VERSION`. */
  version?: string;
  /** Head sampling probability in (0, 1]. Env: `OTEL_TRACES_SAMPLER_ARG`, `TROOPER_SAMPLE_RATE`. Default 1. */
  sampleRate?: number;
  /** `false` turns the SDK off (no providers; helpers become no-ops). Env: `TROOPER_ENABLED=false`. */
  enabled?: boolean;
  /** Logs swallowed SDK errors to stderr. Env: `TROOPER_DEBUG=true`. */
  debug?: boolean;
  /** Extra resource attributes (low cardinality only). */
  resourceAttributes?: Record<string, string | number | boolean>;
  /** Extra OTel instrumentations, registered next to the defaults. */
  instrumentations?: Instrumentation[];
  /**
   * Default instrumentations to skip, by short name: `http`, `undici`, `express`, `koa`, `nestjs`, `kafkajs`,
   * `amqplib`, `pino`, `winston`, `pg`, `mysql2`, `mongodb`, `ioredis`, `redis`. `true` disables all defaults.
   */
  disableInstrumentations?: string[] | true;
  traces?: boolean;
  metrics?: boolean;
  /** Export logs (pino / winston bridge). Default true. */
  logs?: boolean;
  /** Metric export interval. Default 30 000 ms. */
  metricIntervalMs?: number;
  /** Timeout of one export request. Default 10 000 ms. */
  exportTimeoutMs?: number;
  /** Maximum delay before a span/log batch is exported. Default 5 000 ms. */
  batchDelayMs?: number;
  /** Bounded span/log queue; items beyond it are dropped (never blocks). Default 2048. */
  maxQueueSize?: number;
}

export interface ResolvedConfig {
  enabled: boolean;
  debug: boolean;
  service: string;
  environment?: string;
  version?: string;
  sampleRate: number;
  /** Signal URLs; undefined = do not export that signal. */
  tracesUrl?: string;
  metricsUrl?: string;
  logsUrl?: string;
  headers: Record<string, string>;
  traces: boolean;
  metrics: boolean;
  logs: boolean;
  metricIntervalMs: number;
  exportTimeoutMs: number;
  batchDelayMs: number;
  maxQueueSize: number;
  resourceAttributes: Record<string, string | number | boolean>;
  disableInstrumentations: Set<string> | true;
  instrumentations: Instrumentation[];
  /** Why the DSN was rejected, if it was. */
  dsnError?: string;
}

type Env = Record<string, string | undefined>;

function first(...vals: (string | undefined)[]): string | undefined {
  for (const v of vals) {
    const t = v?.trim();
    if (t) return t;
  }
  return undefined;
}

export function parseRate(v: unknown): number | undefined {
  const n = typeof v === 'number' ? v : typeof v === 'string' && v.trim() !== '' ? Number(v.trim()) : NaN;
  return Number.isFinite(n) && n > 0 && n <= 1 ? n : undefined;
}

function parseBool(v: string | undefined): boolean | undefined {
  const t = v?.trim().toLowerCase();
  if (t === 'true' || t === '1' || t === 'yes') return true;
  if (t === 'false' || t === '0' || t === 'no') return false;
  return undefined;
}

/** `k1=v1,k2=v2` (OTEL_EXPORTER_OTLP_HEADERS format, values URL-encoded). */
export function parseHeaders(v: string | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  for (const part of (v ?? '').split(',')) {
    const i = part.indexOf('=');
    if (i <= 0) continue;
    try {
      out[decodeURIComponent(part.slice(0, i).trim())] = decodeURIComponent(part.slice(i + 1).trim());
    } catch {
      /* ignore malformed */
    }
  }
  return out;
}

/**
 * Resolves options. Precedence (.ai/contracts.md §2): explicit option > OTEL_* env (when set) > TROOPER_* env > default.
 * Never throws.
 */
export function resolveConfig(opts: TrooperOptions = {}, env: Env = process.env): ResolvedConfig {
  let dsn: Dsn | undefined;
  let dsnError: string | undefined;
  const dsnStr = first(opts.dsn, env.TROOPER_DSN);
  if (dsnStr) {
    try {
      dsn = parseDsn(dsnStr);
    } catch (e) {
      dsnError = e instanceof Error ? e.message : String(e);
    }
  }
  const base = first(env.OTEL_EXPORTER_OTLP_ENDPOINT, dsn?.endpoint)?.replace(/\/+$/, '');
  const signal = (envKey: string, path: string) => first(env[envKey]) ?? (base ? base + path : undefined);
  const headers: Record<string, string> = { ...parseHeaders(env.OTEL_EXPORTER_OTLP_HEADERS) };
  if (dsn) headers['x-trooper-key'] = dsn.key;

  const enabledEnv = parseBool(env.TROOPER_ENABLED);
  const disabled = opts.enabled === false || (opts.enabled === undefined && enabledEnv === false);
  const traces = opts.traces ?? true;
  const metrics = opts.metrics ?? true;
  const logs = opts.logs ?? true;
  return {
    enabled: !disabled,
    debug: opts.debug ?? parseBool(env.TROOPER_DEBUG) ?? false,
    service: first(opts.service, env.OTEL_SERVICE_NAME, env.TROOPER_SERVICE, env.npm_package_name) ?? 'unknown_service:node',
    environment: first(opts.environment, env.TROOPER_ENV),
    version: first(opts.version, env.TROOPER_VERSION),
    sampleRate: parseRate(opts.sampleRate) ?? parseRate(env.OTEL_TRACES_SAMPLER_ARG) ?? parseRate(env.TROOPER_SAMPLE_RATE) ?? 1,
    tracesUrl: traces ? signal('OTEL_EXPORTER_OTLP_TRACES_ENDPOINT', '/v1/traces') : undefined,
    metricsUrl: metrics ? signal('OTEL_EXPORTER_OTLP_METRICS_ENDPOINT', '/v1/metrics') : undefined,
    logsUrl: logs ? signal('OTEL_EXPORTER_OTLP_LOGS_ENDPOINT', '/v1/logs') : undefined,
    headers,
    traces,
    metrics,
    logs,
    metricIntervalMs: opts.metricIntervalMs && opts.metricIntervalMs > 0 ? opts.metricIntervalMs : 30_000,
    exportTimeoutMs: opts.exportTimeoutMs && opts.exportTimeoutMs > 0 ? opts.exportTimeoutMs : 10_000,
    batchDelayMs: opts.batchDelayMs && opts.batchDelayMs > 0 ? opts.batchDelayMs : 5_000,
    maxQueueSize: opts.maxQueueSize && opts.maxQueueSize > 0 ? opts.maxQueueSize : 2048,
    resourceAttributes: { ...(opts.resourceAttributes ?? {}) },
    disableInstrumentations: opts.disableInstrumentations === true ? true : new Set(opts.disableInstrumentations ?? []),
    instrumentations: opts.instrumentations ?? [],
    dsnError,
  };
}
