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
  /**
   * Continuous profiling (`node:inspector` CPU + sampling heap profiler, folded stacks to `POST /v1/profiles`).
   * Env: `TROOPER_PROFILING`, `TROOPER_PROFILING_TYPES`, `TROOPER_PROFILING_PERIOD`, `TROOPER_PROFILING_CPU_DURATION`.
   * Default off. In AWS Lambda only the explicit option turns it on (the env vars are ignored there).
   */
  profiling?: boolean | ProfilingOptions;
  /** LLM instrumentation defaults (`instrumentOpenAI` / `instrumentAnthropic`). */
  llm?: LlmOptions;
}

/** Profile types the Node SDK can produce (.ai/contracts.md §24). */
export type ProfileType = 'cpu' | 'wall' | 'inuse_space' | 'alloc_space';
export const PROFILE_TYPES: readonly ProfileType[] = ['cpu', 'wall', 'inuse_space', 'alloc_space'];
/** Default Node profile types: on-CPU time of the main thread + live heap. */
export const DEFAULT_PROFILE_TYPES: readonly ProfileType[] = ['cpu', 'inuse_space'];

export interface ProfilingOptions {
  /** Default true when the object form is used. */
  enabled?: boolean;
  /**
   * `cpu` (main-thread on-CPU time, idle samples excluded), `wall` (same samples incl. `[idle]`), `inuse_space` (live heap,
   * sampled), `alloc_space` (bytes allocated during the period, sampled). The V8 heap sampler runs in one mode only: when
   * both heap types are requested `inuse_space` wins. Default `cpu,inuse_space`.
   */
  types?: ProfileType[];
  /** Seconds between profiles. Default 60. */
  periodS?: number;
  /** Length of each CPU profile, seconds (≤ periodS). Default 10. */
  cpuDurationS?: number;
  /** CPU sampling interval, microseconds. Default 10 000 (10 ms). */
  cpuSamplingIntervalUs?: number;
  /** Heap sampling interval, bytes. Default 524 288 (512 KiB). */
  heapSamplingIntervalBytes?: number;
}

export interface ResolvedProfiling {
  types: ProfileType[];
  periodMs: number;
  cpuDurationMs: number;
  cpuSamplingIntervalUs: number;
  heapSamplingIntervalBytes: number;
}

export interface LlmOptions {
  /**
   * Record prompts / completions (`gen_ai.input.messages`, `gen_ai.output.messages`, `gen_ai.system_instructions`).
   * Default false (PII). Env: `TROOPER_LLM_CAPTURE_CONTENT=true`.
   */
  captureContent?: boolean;
  /**
   * OpenAI chat streams only report token usage with `stream_options: { include_usage: true }`. When true the wrapper
   * adds it to streaming requests that don't set `stream_options.include_usage` (the stream then ends with an extra chunk
   * whose `choices` is empty). Default false: the request is never changed and such streams carry no token counts.
   */
  injectStreamUsage?: boolean;
}

export interface ResolvedLlm {
  captureContent: boolean;
  injectStreamUsage: boolean;
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
  /** `POST /v1/profiles` URL (undefined without DSN / endpoint). */
  profilesUrl?: string;
  /** Profiling settings; undefined = profiling off. */
  profiling?: ResolvedProfiling;
  llm: ResolvedLlm;
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

export function parseBool(v: string | undefined): boolean | undefined {
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

function positive(...vals: unknown[]): number | undefined {
  for (const v of vals) {
    const n = typeof v === 'number' ? v : typeof v === 'string' && v.trim() !== '' ? Number(v.trim()) : NaN;
    if (Number.isFinite(n) && n > 0) return n;
  }
  return undefined;
}

function profileTypes(v: unknown): ProfileType[] | undefined {
  const list = Array.isArray(v) ? v : typeof v === 'string' ? v.split(',') : undefined;
  if (!list) return undefined;
  const out: ProfileType[] = [];
  for (const raw of list) {
    const t = String(raw).trim().toLowerCase() as ProfileType;
    if (PROFILE_TYPES.includes(t) && !out.includes(t)) out.push(t);
  }
  return out.length ? out : undefined;
}

/** Profiling config: explicit option > `TROOPER_PROFILING*` env (ignored in AWS Lambda) > off. Never throws. */
export function resolveProfiling(opt: TrooperOptions['profiling'], env: Env = process.env): ResolvedProfiling | undefined {
  const o: ProfilingOptions = typeof opt === 'object' && opt !== null ? opt : {};
  let on: boolean;
  if (typeof opt === 'boolean') on = opt;
  else if (typeof opt === 'object' && opt !== null) on = opt.enabled ?? true;
  else on = !env.AWS_LAMBDA_FUNCTION_NAME && parseBool(env.TROOPER_PROFILING) === true;
  if (!on) return undefined;
  const periodMs = (positive(o.periodS, env.TROOPER_PROFILING_PERIOD) ?? 60) * 1000;
  const cpuMs = (positive(o.cpuDurationS, env.TROOPER_PROFILING_CPU_DURATION) ?? 10) * 1000;
  return {
    types: profileTypes(o.types) ?? profileTypes(env.TROOPER_PROFILING_TYPES) ?? [...DEFAULT_PROFILE_TYPES],
    periodMs: Math.max(1000, periodMs),
    cpuDurationMs: Math.max(100, Math.min(cpuMs, periodMs)),
    cpuSamplingIntervalUs: Math.round(positive(o.cpuSamplingIntervalUs) ?? 10_000),
    heapSamplingIntervalBytes: Math.round(positive(o.heapSamplingIntervalBytes) ?? 512 * 1024),
  };
}

/** LLM config: explicit option > `TROOPER_LLM_CAPTURE_CONTENT` > false. */
export function resolveLlm(opt: LlmOptions | undefined, env: Env = process.env): ResolvedLlm {
  return {
    captureContent: opt?.captureContent ?? parseBool(env.TROOPER_LLM_CAPTURE_CONTENT) ?? false,
    injectStreamUsage: opt?.injectStreamUsage ?? false,
  };
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
    profilesUrl: base ? base + '/v1/profiles' : undefined,
    profiling: resolveProfiling(opts.profiling, env),
    llm: resolveLlm(opts.llm, env),
    dsnError,
  };
}
