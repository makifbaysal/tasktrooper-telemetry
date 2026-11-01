# SDK design

## Principles

1. **One-line install, meaningful data with zero config.** Auto-instrumentation is on by default.
2. **Never break the host.** Exceptions are swallowed; overhead budget: < 2% CPU, < 20 MB memory, < 50 µs per request.
3. **OpenTelemetry compatible.** Backend SDKs wrap the OTel SDK; they add our own exporter + presets. Custom spans the user writes with the OTel API also reach us.
4. **Privacy by default.** Body/header/query are not collected; the IP is converted to geo on the server and dropped; masking via the `beforeSend` hook.
5. **Batch + backoff + offline queue.** In the browser, flush with `sendBeacon` when the page closes; disk queue on mobile.
6. **Presence signal.** Backend SDKs (Go, JVM, .NET, Python, Node), as soon as they start and even without traffic, send the `trooper.sdk.heartbeat`
   gauge (1, meter `TaskTrooper.Telemetry`, no attributes) at the metric interval; first export ≤ 10 s (if the interval is long,
   a one-off, non-blocking flush after ~5 s). Not sent if metrics are off / there is no DSN. Detail: contracts.md §4.

## Packages

Backend SDKs are OpenTelemetry distributions (ADR-011): OTel SDK + our presets (DSN, resource, sampler, `trooper.*`
attributes). Client SDKs are our own lightweight RUM layer and send JSON to `POST /v1/rum`.

| Package | Folder | Target | Scope |
|---|---|---|---|
| `github.com/makifbaysal/tasktrooper-telemetry/sdks/go` (+ `integrations/*` separate modules) | `sdks/go` | Go | net/http (ServeMux pattern), gin, fiber, echo, chi, grpc, franz-go, sarama, amqp091, database/sql, pgx, go-redis, slog/zap |
| `trooper-javaagent` (OTel agent + extension) | `sdks/jvm` | JVM (no code changes) | All auto-instrumentation of the OTel Java agent + trooper presets |
| `trooper-spring-boot-starter` | `sdks/jvm` | Spring Boot 3 | MVC/WebFlux route template, `@KafkaListener`/`@RabbitListener` process name |
| `trooper-quarkus` | `sdks/jvm` | Quarkus 3 | quarkus-opentelemetry config, JAX-RS route template, reactive messaging (`trooper.consumer.attempt` automatic in SmallRye Kafka/RabbitMQ consumers: the `trooper` propagator reads `tt-attempt` → `kafka_deliveryAttempt` → `x-delivery-count`+1 → `x-death`+1 → `delayed-retry-count`+1 during extract) |
| `trooper-llm` | `sdks/jvm` | JVM | GenAI span API (`TrooperLlm`), Anthropic SDK wrapper, LangChain4j listener — ADR-023 |
| `trooper-ktor` + `trooper-kotlin` | `sdks/jvm` | Kotlin / Ktor 3 | server + client plugin, coroutine context |
| `TaskTrooper.Telemetry` (+ `.AspNetCore`, `.Kafka`, `.RabbitMQ`, `.MassTransit`, `.EntityFrameworkCore`, `.Redis`) | `sdks/dotnet` | .NET 8+ | ASP.NET Core, HttpClient, SqlClient/EF Core, Npgsql, Redis, gRPC, Confluent.Kafka, RabbitMQ, MassTransit |
| `@tasktrooper/node` | `sdks/js/packages/node` | Node.js ≥ 22.15 | `@opentelemetry/sdk-node` + OTLP/HTTP protobuf (traces/metrics/logs); http, undici, express, koa, NestJS, kafkajs, amqplib, pg, mysql2, mongodb, ioredis/redis, pino/winston log bridge; route template helpers (express, fastify, koa, Hono, NestJS interceptor), BullMQ processor wrapper, `tt-attempt` read/write |
| `@tasktrooper/core` + `@tasktrooper/browser` | `sdks/js/packages` | Web | page view, SPA route, Web Vitals, fetch/XHR + traceparent, JS error, click (rage/dead), sendBeacon, offline queue |
| `@tasktrooper/replay` | `sdks/js/packages` | Web | session replay (rrweb recording, masking, sampling) — ADR-021 |
| `@tasktrooper/react`, `@tasktrooper/vue`, `@tasktrooper/angular` | `sdks/js/packages` | Web frameworks | ErrorBoundary/errorHandler, router integration (react-router, Next.js, TanStack Router, vue-router, Angular Router) → route template view name |
| `@tasktrooper/react-native` | `sdks/js/packages` | React Native | react-navigation screens, app start, JS errors, network, AppState flush |
| `trooper-android` (+ `-okhttp`, `-compose`) | `sdks/android` | Android | Activity/Fragment/Compose screens, app start, crash, ANR, frozen frame, OkHttp |
| `TaskTrooper` (SPM + CocoaPods) | `sdks/ios` | iOS | UIKit/SwiftUI screens, app start, crash (signal + NSException), hang, URLSession |
| `tasktrooper_flutter` (+ `tasktrooper_dio`) | `sdks/flutter` | Flutter | Navigator/go_router, Flutter/Dart errors, http/dio, frame timing, app start |
| `tasktrooper` (PyPI, ADR-024) | `sdks/python` | Python ≥ 3.9 | OTel distribution (django, fastapi, flask, celery, sqlalchemy, requests, httpx — OTel contrib extras), route template, profiling, `instrument_openai` / `instrument_anthropic` |

Library-specific code is written only for information OTel does not provide: route template (`trooper.route`), consumer process name,
calling service (`tt.svc` baggage). Everything else comes from OTel instrumentation.

**Dogfooding (ADR-027, contracts §27).** The platform itself is the first user of these SDKs: Go services
report with `telemetry-go` (`TROOPER_DSN`), the dashboard `apps/web` with `@tasktrooper/browser` + `@tasktrooper/react` + `@tasktrooper/replay`
(`tt-web`, `VITE_TROOPER_DSN`; if absent the SDK chunk is never loaded — dynamic import), and infrastructure with `trooper-agent`
(`deploy/self/agent.yaml`, `make self`), to a separate "TaskTrooper" project. The platform uses the SDKs from the published
version (ADR-029): Go services `v0.1.0` from proxy.golang.org, `apps/web` `^0.1.0` from npm.

## API surface

### Node

```ts
// ESM: node --import @tasktrooper/node/register app.mjs   (CJS: --require; initializes from env)
import { init } from '@tasktrooper/node';

init({
  dsn: process.env.TROOPER_DSN,           // https://<ingest-key>@ingest.trooper.dev/<project>
  service: 'orders-api',
  environment: 'production',
  version: process.env.GIT_SHA,           // deploy marker
  sampleRate: 1.0,                        // ParentBased(TraceIdRatio); root span writes tt=r:<rate> to tracestate
});
```

`init` is idempotent and never throws; without a DSN it does not export (propagation works). Config precedence contracts.md §2:
explicit > `OTEL_*` > `TROOPER_*`. Resource: `service.*`, `deployment.environment.name`, `telemetry.distro.name=trooper-node`.
Span processor: the local-root SERVER/CONSUMER span becomes the entry; `trooper.entry.route/method` is copied to child spans (the route
is resolved via a lazy resolver / OTel `RPCMetadata` / `http.route`, because the framework router matches it after the span starts),
`trooper.caller` from the `tt.svc` baggage, `trooper.sample_rate` from upstream `tt=r:`. Query values are removed from URL attributes.

Route template helpers: `app.use(trooperExpress())`, `app.register(trooperFastify)`, `trooperKoa()`, `trooperHono()`,
`app.useGlobalInterceptors(new TrooperInterceptor())` (NestJS). Manually: `setRoute('/users/:id')`.

Additional API for workers/consumers:

```ts
import { processMessage, instrumentBullmqProcessor, withTraceContext, injectHeaders, nextAttempt, setAttemptHeader, markDlq } from '@tasktrooper/node';

// kafkajs / amqplib: upstream OTel instrumentation + hook → trooper.consumer.attempt (tt-attempt, x-delivery-count, x-death)
new Worker('emails', instrumentBullmqProcessor(async (job) => send(job.data)));     // attempt = attemptsStarted
await queue.add('welcome', withTraceContext({ to }));                                // no headers in BullMQ → job data
await processMessage({ system: 'kafka', destination: 'orders', consumerGroup: 'billing', headers }, handler);
producer.send({ topic: 'orders.retry', messages: [{ value, headers: setAttemptHeader(injectHeaders(), nextAttempt()) }] });
```

Every swallowed SDK error is counted as `trooper.sdk.errors{component}` (meter `TaskTrooper.Telemetry`); export errors
are `component=exporter.<signal>`.

### Go

```go
shutdown, _ := trooper.Start(ctx, trooper.Config{Service: "billing-worker"}) // DSN: TROOPER_DSN
defer shutdown(context.Background())

// Background work (scheduler tick, poll that finds work, batch) = job entry (contracts §4):
// kind consumer, trooper.process = trooper.entry.route = name, trooper.entry.method = JOB; child spans attach to the job.
err := trooper.RunJob(ctx, "billing.sync", trooper.PanicRepanic, func(ctx context.Context) error { return sync(ctx) })

ctx, span := trooper.StartJob(ctx, "invoice.send", trace.WithAttributes(attribute.Int64("tenant.id", id)))
defer trooper.EndSpan(span, err)
```

- The name must be low-cardinality (tenant/id go in attributes). Started only on a run that does work; an empty poll produces no span.
- A local span in ctx is not made the parent (new root + link: a job is always an entry); a remote parent (e.g. `TRACEPARENT`) is preserved.
- Without a provider it returns a no-op span and does not panic. `RunJob` writes the panic to the span (`PanicRepanic` / `PanicRecover`).
- For message consumption use `ProcessMessage` / `StartConsumer` (with messaging attributes), not a job.

**Heartbeat (contracts §4):** with metrics on, `Start` registers the `trooper.sdk.heartbeat` gauge (1, meter `TaskTrooper.Telemetry`);
the first export happens via a one-off `ForceFlush` ~5 s after `Start` (without waiting for the 30 s `MetricInterval`), then at every
metric interval. A random per-process `service.instance.id` in the resource (UUIDv4; `OTEL_RESOURCE_ATTRIBUTES` /
`ResourceAttributes` override it).

### Browser

```ts
import { init, identify, track } from '@tasktrooper/browser';

init({ dsn, service: 'shop-web', environment: 'production', version, tracePropagationTargets: [/api\.shop\.com/] });
identify({ id: user.id, email: user.email, plan: 'pro' });   // opt-in
track('checkout_started', { cart_value: 120 });
```

`tracePropagationTargets` → a `traceparent` header is added to fetch/XHR, joining the frontend view with the backend trace.

Client SDKs (browser, React Native, Android OkHttp, iOS URLSession, Flutter http/Dio) also add `baggage: tt.svc=<app>` to every request
they add `traceparent` to (value percent-encoded; an existing `baggage` header is preserved, an old `tt.svc` is replaced, total ≤ 8192 bytes).
The backend reads this as `trooper.caller`, so the call shows up with the app name instead of the generic `web`/`mobile`. Cross-origin customer APIs
must add `baggage` alongside `traceparent` to CORS `Access-Control-Allow-Headers`.

### Session replay (ADR-021, contracts §23)

```ts
import { init } from '@tasktrooper/browser';
import { replayIntegration } from '@tasktrooper/replay';

init({ dsn, service: 'shop-web', integrations: [
  replayIntegration({ sampleRate: 0.1, onErrorSampleRate: 1.0, maskAllText: true, maskAllInputs: true }),
] });
```

`@tasktrooper/replay` wraps `@rrweb/record` (separate package; `@tasktrooper/browser` size is unchanged). CDN: `trooper-replay.iife.js`
`window.TrooperReplay`. It gets session and error info from the integration hooks of `@tasktrooper/browser`.

React Native: `import { startReplay } from '@tasktrooper/react-native'; startReplay({ sampleRate: 0.1 })` (requires the native SDK).
Flutter: `TaskTrooper.init(config, appRunner: …, replay: const TrooperReplayOptions(sampleRate: 0.1))` — wireframe on the Dart side; unmask `TrooperUnmask(child: …)`.

Mobile (wireframe, no screenshots):
- Android: `TrooperReplay.install(context, ReplayOptions(sampleRate = 0.1, onErrorSampleRate = 1.0))` (`trooper-replay` module; offline queue ≤ 2 MB, when full new segments are dropped);
  for Compose via `trooper-compose` semantics. Unmask: `view.trooperUnmask()`, `Modifier.trooperUnmask()`.
- iOS: `TaskTrooper.start(config, replay: .init(sampleRate: 0.1, onErrorSampleRate: 1.0))` (`TaskTrooperReplay` target,
  pod `TaskTrooper/Replay`; on crash the on-error buffer cannot be flushed — signal handler); unmask
  `view.trooperUnmask = true`, SwiftUI `.trooperUnmask()`.

### Continuous profiling (ADR-022, contracts §24)

Off by default; `TROOPER_PROFILING=true` or in code:
- Go: `trooper.Start(ctx, trooper.Config{…}, trooper.WithProfiling())` — `runtime/pprof` (CPU and goroutine pprof; since heap/mutex/block are cumulative, the per-period delta is sent as folded, the first period is baseline only); on HTTP/gRPC/consumer entries the goroutine gets
  `trooper.route` / `trooper.method` pprof labels → per-endpoint flame graph.
- JVM: `-Dtrooper.profiling.enabled=true` (javaagent extension; JFR `RecordingStream`, JDK 17+). Spring Boot starter
  `trooper.profiling.enabled`. Continuous sampling (CPU 20 ms; `CPU_DURATION` is ignored), default types `cpu,alloc_space,lock`
  (no in-use heap in JFR). Route label via thread → entry span mapping (`# trooper.route=`).
- Node: `init({ profiling: true })` — `node:inspector` CPU + sampling heap profiler, in folded format.
- .NET: `TaskTrooper.Telemetry.Profiling` package, `services.AddTaskTrooperProfiling()` (EventPipe, in-process).
- Python: `tasktrooper.init(profiling=True)` (sampler thread; `TROOPER_PROFILING=true`).
- Any language: OTLP profiles (`/v1development/profiles`) — e.g. OTel eBPF profiler / Collector.

### AI / LLM (ADR-023, contracts §25)

LLM calls are sent as OTel GenAI spans; content (`captureContent`) is off by default.
- Node: `instrumentOpenAI(new OpenAI())`, `instrumentAnthropic(new Anthropic())` (tokens + TTFT, streams included); Vercel AI SDK
  `experimental_telemetry: { isEnabled: true }` spans come through as-is. `init({ llm: { captureContent: true } })`.
- Go (`github.com/makifbaysal/tasktrooper-telemetry/sdks/go/llm`): `client := openai.NewClient(option.WithHTTPClient(llm.HTTPClient(nil)))` / same for Anthropic (`llm.HTTPClient` recognizes known
  provider hosts, reads model + tokens from the request/response JSON, counts SSE streams without breaking them); manually:
  `ctx, call := llm.Start(ctx, llm.Request{Provider: "anthropic", Model: m, Operation: "chat"}); defer call.End(llm.Usage{…})`.
- JVM (`trooper-llm` module — only `opentelemetry-api`, visible from application code alongside the agent; included in the Spring starter):
  Spring AI spans are normalized, `TrooperChatModelListener` for LangChain4j, Anthropic Java SDK `TrooperLlm.wrap(client)`,
  OpenAI Java SDK via the javaagent's upstream instrumentation; manually `TrooperLlm.start(provider, model)` → `LlmCall`.
- .NET: `Microsoft.Extensions.AI`, `OpenAI.*`, `Azure.AI.Inference*`, `Microsoft.SemanticKernel*` ActivitySources are listened to by
  default (`EnableLlm`, `LlmCaptureContent`); manually `TrooperLlm.Start(provider, model, op)`.

### Serverless (AWS Lambda, Azure Functions — ADR-020)

Metrics (invocations, errors, throttles, duration, concurrency …) come without an SDK: the AWS/Azure integration
(`services/integrations`, contracts §21) pulls them from CloudWatch / Azure Monitor. For **traces and cold starts** the function is instrumented
with OTel; the spans' `service.name` must match `serverless_functions.service` (default function /
app name; override: Lambda tag or Function App tag `tasktrooper.service`).

**Azure per-function metrics:** if an Application Insights component is linked to the Function App (`hidden-link` tag),
the integration opens each function as a separate row (`azure-<app>-<function>`) and pulls its metrics from the App Insights `requests`
table (ARM proxy, no extra role needed); p95/p99 come too. `service` = the function's most frequent `cloud_RoleName` — for span
matching keep it equal to `OTEL_SERVICE_NAME` (or the cloud role name in App Insights). Optional
`config.readAppSettings: true` (requires Website Contributor) reads the `TASKTROOPER_SERVICE` / `OTEL_SERVICE_NAME` app setting
and overrides `service`; without permission it is silently skipped, no other setting is stored. Detail: contracts §22,
`services/integrations/README.md` § Azure.

**Node.js Lambda — `@tasktrooper/node`:**

```ts
import { init, wrapLambda } from '@tasktrooper/node';
init({ service: 'orders-api' });                       // at module level; DSN from env (TROOPER_DSN)
export const handler = wrapLambda(async (event, context) => ({ statusCode: 200, body: 'ok' }));
```

`wrapLambda(handler, opts?)`: `faas.coldstart=true` on the first invocation (then `false`), `faas.invocation_id` = `awsRequestId`,
`faas.name` = `AWS_LAMBDA_FUNCTION_NAME`, `faas.trigger` from the event shape (API Gateway / Function URL / ALB → `http`,
SQS/SNS/Kinesis/EventBridge event → `pubsub`, DynamoDB/S3 → `datasource`, EventBridge schedule → `timer`), `cloud.provider=aws`.
SERVER span for HTTP events (`GET /users/:id`, `traceparent` continues), CONSUMER span for queue batches. Flushes before
returning (at most `flushTimeoutMs`, default 2 s); supports async and callback handlers; telemetry errors never
reach the handler.

**Other languages — OTel / ADOT Lambda layer:** language layer (e.g. `aws-otel-python-*`, `opentelemetry-nodejs-*`) + env:

```bash
AWS_LAMBDA_EXEC_WRAPPER=/opt/otel-instrument          # node/java: /opt/otel-handler
OTEL_SERVICE_NAME=orders-api
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_ENDPOINT=https://ingest.trooper.dev
OTEL_EXPORTER_OTLP_HEADERS=x-trooper-key=<ingest key>
OTEL_RESOURCE_ATTRIBUTES=deployment.environment.name=production
```

If the collector layer (ADOT) is used, the SDK sends to `localhost:4318`; in the collector config (`OPENTELEMETRY_COLLECTOR_CONFIG_URI`)
the `otlphttp` exporter is defined with `endpoint: https://ingest.trooper.dev`, `headers: {x-trooper-key: <ingest key>}`.
The OTel semconv Lambda instrumentation writes `faas.coldstart` / `faas.trigger` / `faas.invocation_id` itself.

**Azure Functions — OpenTelemetry mode:** `host.json`:

```json
{ "version": "2.0", "telemetryMode": "OpenTelemetry" }
```

App settings (Function App → Environment variables):

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=https://ingest.trooper.dev
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_HEADERS=x-trooper-key=<ingest key>
OTEL_SERVICE_NAME=billing-func                         # = Function App name or tag tasktrooper.service
```

OTel is also enabled on the worker side (.NET isolated: `builder.Services.AddOpenTelemetry().UseFunctionsWorkerDefaults().UseOtlpExporter()`;
Node: `@azure/functions-opentelemetry-instrumentation` + OTLP exporter; Python: `PYTHON_ENABLE_OPENTELEMETRY=true`).
Host spans carry `faas.trigger`, `faas.invocation_id` (and `faas.coldstart` if the host supports it); the cold start count in the api
is computed from entry spans with `faas.coldstart='true'`.

## Wire format

- Backend: **OTLP/HTTP protobuf** → `POST /v1/traces`, `/v1/metrics`, `/v1/logs`. Header: `x-trooper-key`.
- Replay segments → `POST /v1/replay` (contracts §23); profiles → `POST /v1/profiles` (pprof or folded, §24).
- Browser/mobile: **RUM batch JSON** → `POST /v1/rum` (gzip). The schema is defined in `proto/rum/v1/rum.proto`, its JSON mapping is used (field names snake_case, enums as names).

RUM event types: `view_start`, `view_end`, `vital`, `resource`, `error`, `action`, `custom`, `session_start`.
Common fields: `session_id`, `view_id`, `anonymous_id`, `user_id?`, `ts`, `service`, `version`, `env`, `device`, `connection`.

## Semantic conventions

OTel semantic conventions are the basis. Full list of `trooper.*` attributes and how the writer reads them:
[`contracts.md` § 4](contracts.md#4-semantic-conventions-sdk--writer). Summary: `trooper.route`, `trooper.entry.route`
(on all spans of the request), `trooper.process`, `trooper.caller` (baggage `tt.svc`), `trooper.consumer.attempt`,
`trooper.consumer.dlq`, `trooper.sample_rate`.

## Versioning

Semver. Breaking change in the wire protocol → new endpoint version (`/v2/rum`), the old version is supported for at least 12 months.

## Distribution

SDKs are published to public registries (ADR-029, license Apache-2.0). Each ecosystem is versioned independently; releases are done with an
ecosystem-specific tag (`js-vX.Y.Z`, `jvm-vX.Y.Z`, `android-vX.Y.Z`, `dotnet-vX.Y.Z`, `python-vX.Y.Z`, `ios-vX.Y.Z`,
`tasktrooper_flutter-vX.Y.Z`, `tasktrooper_dio-vX.Y.Z`, `sdks/go/vX.Y.Z`) via `.github/workflows/release-*.yml`.
The workflow compares the tag version with the package version, runs the tests, then publishes. Authentication is OIDC
trusted publishing wherever possible (npm, PyPI, NuGet, pub.dev); Maven Central and CocoaPods use a repo secret.

| Ecosystem | Install |
|---|---|
| Go | `go get github.com/makifbaysal/tasktrooper-telemetry/sdks/go@latest` |
| JS | `npm i @tasktrooper/browser` (etc.) |
| JVM / Android | `implementation("ai.tasktrooper:<artifact>:<v>")` + `mavenCentral()` |
| .NET | `dotnet add package TaskTrooper.Telemetry` |
| Python | `pip install tasktrooper` |
| Flutter | `tasktrooper_flutter: ^<v>` |
| iOS | SwiftPM `https://github.com/makifbaysal/tasktrooper-telemetry` or `pod 'TaskTrooper'` |

The platform itself (services, `apps/web`) also uses the registry packages; an SDK change only reaches the platform once a new version is published
and the dependency is bumped. SDK examples (`sdks/*/examples`) stay on local paths.
