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
| `profiling` (`true` or `{ types, periodS, cpuDurationS, cpuSamplingIntervalUs, heapSamplingIntervalBytes }`) | `TROOPER_PROFILING`, `TROOPER_PROFILING_TYPES`, `TROOPER_PROFILING_PERIOD`, `TROOPER_PROFILING_CPU_DURATION` | off; `cpu,inuse_space`, 60 s, 10 s, 10 ms, 512 KiB |
| `llm.captureContent` | `TROOPER_LLM_CAPTURE_CONTENT` | `false` |
| `llm.injectStreamUsage` | — | `false` |

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

### AWS Lambda

```ts
import { init, wrapLambda } from '@trooper/node';

init({ service: 'orders' });   // module scope, once per execution environment (TT_DSN / OTLP env as usual)

export const handler = wrapLambda(async (event, context) => {
  return { statusCode: 200, body: 'ok' };
});
// callback handlers work too: wrapLambda((event, context, callback) => callback(null, result))
```

`wrapLambda(handler, opts?)` runs every invocation in an entry span with the OTel FaaS conventions
(.ai/contracts.md §21):

| Attribute | Value |
|---|---|
| `faas.name` | `opts.name` → `AWS_LAMBDA_FUNCTION_NAME` → `context.functionName` |
| `faas.coldstart` | `true` on the first invocation of the execution environment, then `false` |
| `faas.invocation_id` | `context.awsRequestId` |
| `faas.trigger` | `opts.trigger` or inferred: API Gateway (REST/HTTP API), Function URL, ALB → `http`; SQS/SNS/Kinesis/Kafka/MQ/EventBridge events → `pubsub`; DynamoDB streams, S3 → `datasource`; EventBridge schedules → `timer`; else `other` |
| `cloud.provider` / `cloud.platform` / `cloud.region` | `aws` / `aws_lambda` / `AWS_REGION` |
| `faas.version`, `faas.max_memory`, `cloud.resource_id` | function version, memory (bytes), invoked ARN |

HTTP events give a SERVER span `GET /users/:id` (route from `routeKey` / `resource`, else the normalized path;
`http.response.status_code` from the result, ≥ 500 = error) that continues the caller's `traceparent` header. SQS / SNS /
Kinesis batches give a CONSUMER span `process <queue>` (`messaging.*`, `trooper.process` = function name).
Before the wrapper resolves it flushes traces/metrics/logs (`getHandle().forceFlush()`, bounded by
`opts.flushTimeoutMs`, default 2000 ms) — Lambda freezes the process after the handler returns. Handler errors are
recorded and re-thrown unchanged; telemetry failures (including a slow or failing flush) never surface. Options:
`name`, `trigger`, `flush` (default `true`), `flushTimeoutMs`, `propagate` (default `true`), `flushFn`.

The span's `service.name` must match the function's `service` in TaskTrooper (default: function name; override with
the Lambda tag `tasktrooper.service`), so cold starts and errors line up with the CloudWatch metrics.

## Continuous profiling

```ts
init({ dsn, service: 'orders-api', profiling: true });
// or: profiling: { types: ['cpu', 'wall', 'inuse_space'], periodS: 60, cpuDurationS: 10 }
// or: TROOPER_PROFILING=true TROOPER_PROFILING_TYPES=cpu,inuse_space node --import @trooper/node/register app.mjs
```

Every `periodS` (default 60 s) the SDK takes a `cpuDurationS` (default 10 s, 10 ms sampling) CPU profile with
`node:inspector` and reads the V8 sampling heap profiler, converts both to **folded stacks** (root first, `;`-separated)
and uploads each type to `POST /v1/profiles?format=folded&type=…&unit=…&service=…&env=…&version=…&host=…&runtime=node&start=…&end=…`
(gzip, `text/plain`, `x-trooper-key`; .ai/contracts.md §24). The flame graph lives on the service / endpoint pages.

| Type | Unit | What it is |
|---|---|---|
| `cpu` (default) | nanoseconds | samples of the **main JS thread** with `(idle)` excluded → on-CPU time (JS, GC as `[gc]`, native VM time as `[program]`). Each sample lasts until the next one. |
| `wall` | nanoseconds | the same samples incl. idle time as an `[idle]` stack (opt-in) |
| `inuse_space` (default) | bytes | live heap of sampled allocations (`HeapProfiler.startSampling`, 512 KiB interval) |
| `alloc_space` | bytes | bytes allocated during the period (sampler keeps collected objects); V8 runs one heap sampler, so when both heap types are requested `inuse_space` wins |

Frame names are `fn (relative/path.js)`: cwd and `file://` are stripped, no line/column, `(anonymous)` for unnamed
functions, `node:` builtins keep their URL (`readFileSync (node:fs)`); `(root)` is dropped; stacks deeper than 128 frames
keep the leaf side under a `[truncated]` frame. Timers are `unref()`ed; `shutdown()` stops the profiler and uploads the
CPU profile in progress. There is no queue: a profile that fails to upload is dropped, 429/5xx/network errors back off
(1 s → 60 s with jitter, `Retry-After` honoured), every failure counts as `trooper.sdk.errors{component=exporter.profiles}`.
Upload requests are not traced (tracing is suppressed). In AWS Lambda the env vars are ignored — profiling only runs when
`init({ profiling })` asks for it explicitly (a frozen sandbox can't keep a sampling timer). Worker threads are not
profiled (the inspector session belongs to the main thread). Endpoint (`trooper.route`) labels are not attached to Node
stacks yet.

## AI / LLM monitoring

```ts
import OpenAI from 'openai';
import Anthropic from '@anthropic-ai/sdk';
import { init, instrumentOpenAI, instrumentAnthropic } from '@trooper/node';

init({ service: 'support-bot', llm: { captureContent: false } });
const openai = instrumentOpenAI(new OpenAI());          // also AzureOpenAI / OpenAI-compatible base URLs
const anthropic = instrumentAnthropic(new Anthropic());
```

Both return a **proxy** of the client — nothing global is patched, the original object stays uninstrumented, every other
member works as before. Instrumented: OpenAI `chat.completions.create` / `.parse`, `responses.create` / `.parse`,
`embeddings.create`; Anthropic `messages.create`, `messages.stream()` (+ `beta.messages.*`). Each call is a CLIENT span
`"<operation> <model>"` (`chat gpt-4o`, `embeddings text-embedding-3-small`) with the OTel GenAI attributes
(.ai/contracts.md §25): `gen_ai.provider.name`, `gen_ai.operation.name`, `gen_ai.request.model` / `gen_ai.response.model`,
`gen_ai.usage.input_tokens` / `output_tokens` / `cache_read.input_tokens` / `cache_creation.input_tokens`,
`gen_ai.request.max_tokens` / `temperature` / `top_p` / `stop_sequences`, `gen_ai.response.finish_reasons`,
`gen_ai.response.id`, `server.address`. The SDK's HTTP span becomes a child of the LLM span, so the writer does not count
the call twice. `gen_ai.usage.input_tokens` is the **total** input (Anthropic: `input_tokens` + cache reads + cache
writes, like OpenAI's `prompt_tokens`); the cache counts are reported separately.

- **Streaming** (`stream: true`, `messages.stream()`): `trooper.llm.ttft_ms` is set at the first non-empty delta; usage,
  finish reason and (with content capture) the output are accumulated from the events; the span ends when the stream is
  exhausted, `break`s or fails. OpenAI chat streams only report usage when the request has
  `stream_options: { include_usage: true }` — the wrapper does **not** change your request unless you opt in with
  `init({ llm: { injectStreamUsage: true } })` (or `instrumentOpenAI(client, { injectStreamUsage: true })`), because the
  extra final chunk has an empty `choices` array that code reading `chunk.choices[0]` must tolerate.
- **Errors**: span status ERROR, exception event, `error.type` (provider error type such as `rate_limit_error` /
  `overloaded_error`, else the error class) and `http.response.status_code`; the original error is re-thrown unchanged.
- **Content** is off by default (PII). `init({ llm: { captureContent: true } })` / `TROOPER_LLM_CAPTURE_CONTENT=true` (or
  per client: `instrumentOpenAI(client, { captureContent: true })`) records `gen_ai.system_instructions` (system /
  developer messages, Anthropic `system`, Responses `instructions`), `gen_ai.input.messages` and `gen_ai.output.messages`
  as GenAI JSON (`[{ role, parts: [{ type: 'text', content }, { type: 'tool_call', … }] }]`), capped at 32 KB each.
  Inline images / audio / files are never copied (only `{ type: 'blob', modality }`, or the URL for remote images).
  Embedding inputs are never recorded.
- `provider` option overrides `gen_ai.provider.name`; otherwise OpenAI clients derive it from `baseURL` (Azure →
  `azure.ai.openai`, Groq, DeepSeek, xAI, Mistral, Gemini, Ollama `:11434`).
- Not instrumented: OpenAI `chat.completions.stream()` / `runTools()` / `responses.stream()` helpers (they call the
  un-proxied client internally — use `create({ stream: true })`), and streams consumed only through `tee()`.

**Vercel AI SDK**: enable `experimental_telemetry: { isEnabled: true }` on `generateText` / `streamText` / `embed` …;
its spans go through the normal OTel pipeline (the trooper span processor never drops spans). Its provider-call spans
(`ai.*.doGenerate` / `doStream` / `doEmbed`) lack `gen_ai.operation.name`, so the processor fills in
`gen_ai.operation.name`, `gen_ai.provider.name` (`anthropic.messages` → `anthropic`, `google.vertex.chat` →
`gcp.vertex_ai` …) and `trooper.llm.ttft_ms` (from `ai.response.msToFirstChunk`). Its prompt recording follows the AI
SDK's own `recordInputs` / `recordOutputs` settings, not `captureContent`.

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
                                        # error swallowing, e2e (express + pino → fake OTLP server, protobuf decoded),
                                        # profiling (folded conversion, upload, real inspector), LLM wrappers (fakes)
pnpm --filter @trooper/node build       # tsup: ESM + CJS + .d.ts (index, register)
pnpm --filter @trooper/node typecheck
```
