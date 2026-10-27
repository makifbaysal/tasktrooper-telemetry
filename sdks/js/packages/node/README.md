# @trooper/node

TaskTrooper Telemetry distribution of OpenTelemetry for Node.js (≥ 22.15). It configures `@opentelemetry/sdk-node` to
export traces, metrics and logs as **OTLP/HTTP protobuf** to the TaskTrooper ingest gateway and adds the `trooper.*`
conventions (`.ai/contracts.md` §4): route templates, entry route on every span, consumer process / attempt, caller
service, sample rate.

## Install & start

```bash
pnpm add @trooper/node
TROOPER_DSN=https://<ingest-key>@ingest.example.com/<project> TROOPER_SERVICE=orders-api \
  node --import @trooper/node/register app.mjs     # ESM (CJS: node --require @trooper/node/register app.js)
```

Or in code, **before** importing the frameworks it instruments:

```ts
import { init } from '@trooper/node';

const trooper = init({
  dsn: process.env.TROOPER_DSN,       // https://<ingest-key>@host[:port]/<project>
  service: 'orders-api',
  environment: 'production',
  version: process.env.GIT_SHA,       // deploy marker
  sampleRate: 1,                      // head sampling (ParentBased(TraceIdRatio))
});
// await trooper.shutdown() on SIGTERM if your process handles signals (flushes pending telemetry)
```

`init` is idempotent and never throws. Without a DSN (or `OTEL_EXPORTER_OTLP_ENDPOINT`) nothing is exported, but context
propagation still works.

| Option | Env | Default |
|---|---|---|
| `dsn` | `TROOPER_DSN` (`OTEL_EXPORTER_OTLP_ENDPOINT` overrides the endpoint; the DSN key is still sent) | — |
| `service` | `OTEL_SERVICE_NAME`, `TROOPER_SERVICE` | `npm_package_name` / `unknown_service:node` |
| `environment` | `TROOPER_ENV` | — |
| `version` | `TROOPER_VERSION` | — |
| `sampleRate` | `OTEL_TRACES_SAMPLER_ARG`, `TROOPER_SAMPLE_RATE` | `1` |
| `enabled` | `TROOPER_ENABLED=false` | `true` |
| `debug` | `TROOPER_DEBUG=true` (log swallowed SDK errors to stderr) | `false` |
| `traces` / `metrics` / `logs` | per-signal `OTEL_EXPORTER_OTLP_<SIGNAL>_ENDPOINT` | `true` |
| `disableInstrumentations` | — | `[]` (`true` = none of the defaults) |
| `instrumentations` | — | extra OTel instrumentations |
| `metricIntervalMs`, `exportTimeoutMs`, `batchDelayMs`, `maxQueueSize` | — | 30 s, 10 s, 5 s, 2048 |

Precedence: explicit option > `OTEL_*` > `TROOPER_*` > default. Resource: `service.name`, `service.version`,
`deployment.environment.name`, `telemetry.distro.name=trooper-node`, `telemetry.distro.version` (+ OTel env/host/process
detectors).

## What you get

Default instrumentations (upstream OTel): `http`, `undici` (fetch), `express`, `koa`, `nestjs`, `kafkajs`, `amqplib`,
`pino`, `winston` (log correlation + log export), `pg`, `mysql2`, `mongodb`, `ioredis`, `redis`.

The trooper span processor:

- **entry spans** (local-root SERVER / CONSUMER): `trooper.sample_rate` (own rate for roots; the upstream root's rate from
  tracestate `tt=r:<rate>` for remote parents), `trooper.caller` from W3C baggage `tt.svc`, `trooper.route` (server),
  `trooper.process` + `trooper.consumer.attempt` (consumer);
- **every span of the request / message**: `trooper.entry.route` + `trooper.entry.method` (`GET` …, `CONSUME`, `GRPC`).
  Routers match after the HTTP span started, so the route is resolved lazily (framework helper → OTel `RPCMetadata` →
  `http.route`) and filled in when spans end;
- route templates normalized like every trooper SDK: `{id}`, `<id>`, `:id(\\d+)` → `:id`; `{p...}`, `{*p}` → `*p`;
  raw numeric / uuid / hex / ulid segments → `:id`;
- **privacy**: query *values* are removed from `url.full`, `url.query`, `http.target`, `http.url`; no headers or bodies.

The sampler is `ParentBased(TraceIdRatio(rate))` and writes `tt=r:<rate>` into the tracestate of root spans. The
propagator is W3C tracecontext + baggage and sets `tt.svc=<service>` on every outgoing call.

### Route templates

```ts
app.use(trooperExpress());                         // express 4/5 — first middleware; handles mounted routers
await fastify.register(trooperFastify);            // fastify (request.routeOptions.url)
app.use(trooperKoa());                             // koa + @koa/router (ctx._matchedRoute)
app.use(trooperHono());                            // Hono on @hono/node-server
app.useGlobalInterceptors(new TrooperInterceptor()); // NestJS, express or fastify adapter
setRoute('/v1/orders/:id');                         // anything else
```

### Consumers / workers

```ts
import { instrumentBullmqProcessor, withTraceContext, processMessage, injectHeaders,
         setAttemptHeader, nextAttempt, currentAttempt, setProcessName, markDlq } from '@trooper/node';

// kafkajs / amqplib consumers are traced by the upstream instrumentations; trooper hooks add the attempt:
//   tt-attempt → kafka_deliveryAttempt → x-delivery-count + 1 → x-death counts + 1 (amqplib: redelivered → 2), else 1.
// Inside a handler:
setProcessName('order-projector');                  // trooper.process (default: consumer group / topic / queue)
await producer.send({ topic: 'orders.retry',
  messages: [{ value, headers: setAttemptHeader(injectHeaders(), nextAttempt()) }] });
markDlq();                                          // trooper.consumer.dlq=true

// BullMQ (no message headers: the trace context travels in job data under `_trooper`)
await queue.add('welcome', withTraceContext({ to: 'a@b.c' }));
new Worker('emails', instrumentBullmqProcessor(async (job) => send(job.data)), { connection });

// Any other source
await processMessage({ system: 'sqs', destination: 'orders', headers: msg.attributes }, () => handle(msg));
```

## Guarantees

- **Never crashes the host** (AGENTS.md rule 5): helpers, hooks, the span processor, sampler and propagator swallow
  their own errors; errors thrown by *your* handlers are recorded on the span and re-thrown unchanged. Swallowed errors
  are counted in `trooper.sdk.errors{component}` (meter `TaskTrooper.Telemetry`) and `internalErrorCount()`; failed
  export batches count as `component=exporter.traces|metrics|logs`.
- **Async, batched, bounded**: batch span/log processors (bounded queue, drops instead of blocking), periodic metric
  reader, upstream OTLP retry (429/502/503/504 with backoff).
- **No PII by default**: no headers, bodies or query values.

## Limitations

- ESM apps need the loader hook: use `--import @trooper/node/register` (synchronous `module.registerHooks()` loader of
  import-in-the-middle where supported). The `--require` entry uses the asynchronous `module.register()` loader, which
  Node 26 reports as deprecated.
- fastify / Hono / BullMQ have no upstream OTel instrumentation in the default set; HTTP spans come from the `http`
  instrumentation and the helpers add the route; BullMQ needs `instrumentBullmqProcessor`.
- kafkajs does not expose the consumer group to hooks: `trooper.process` defaults to the topic (use `setProcessName`).
- NestJS microservice (`rpc`) contexts are not mapped; use `setProcessName` in the handler.

## Development

```bash
pnpm --filter @trooper/node test        # vitest: routes, config/DSN, sampler/tracestate, baggage, attempts,
                                        # error swallowing, e2e (express + pino → fake OTLP server, protobuf decoded)
pnpm --filter @trooper/node build       # tsup: ESM + CJS + .d.ts (index, register)
pnpm --filter @trooper/node typecheck
```
