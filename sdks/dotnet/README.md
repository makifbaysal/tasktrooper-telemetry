# TaskTrooper Telemetry — .NET SDK

Thin [OpenTelemetry .NET](https://github.com/open-telemetry/opentelemetry-dotnet) distribution (ADR-011). One call configures
resource, sampling, the `trooper.*` conventions (`.ai/contracts.md` §4) and OTLP/HTTP protobuf export of **traces, metrics and
logs** (`ILogger`) to TaskTrooper ingest. Spans you create with `ActivitySource` and metrics from `Meter` are exported too.

Targets `net8.0`, `net9.0`, `net10.0`. OpenTelemetry 1.20.

| Package | Purpose |
|---|---|
| `TaskTrooper.Telemetry` | Core: `AddTaskTrooper()`, DSN, OTLP export with retry, sampler, entry route / caller propagation, HttpClient + SqlClient instrumentation; listens to Npgsql, RabbitMQ.Client 7, MassTransit and GenAI (Microsoft.Extensions.AI, OpenAI) sources when present; `TrooperLlm` manual LLM spans |
| `TaskTrooper.Telemetry.AspNetCore` | Server spans (minimal APIs, controllers, MVC, gRPC services) with `trooper.route`, caller, exceptions; Kestrel/hosting metrics |
| `TaskTrooper.Telemetry.Kafka` | Confluent.Kafka consumer/producer helpers (entry spans, header propagation) |
| `TaskTrooper.Telemetry.RabbitMQ` | RabbitMQ.Client 7 consumer conventions, attempt from `x-delivery-count`/`x-death`, `tt.svc` on publish |
| `TaskTrooper.Telemetry.MassTransit` | Maps MassTransit diagnostics to `trooper.process` (no MassTransit dependency) |
| `TaskTrooper.Telemetry.EntityFrameworkCore` | EF Core command spans (OTel instrumentation, beta) |
| `TaskTrooper.Telemetry.Redis` | StackExchange.Redis spans (OTel instrumentation, beta) |

## Quick start — ASP.NET Core

```csharp
builder.Services
    .AddTaskTrooper(o => { o.Dsn = "https://<key>@ingest.trooper.dev/<project>"; o.ServiceName = "orders-api"; })
    .AddAspNetCore();
```

Server spans get `trooper.route` from the matched endpoint's route template, normalized (`/v1/orders/{id:int}` → `/v1/orders/:id`;
conventional MVC routes resolve `{controller}`/`{action}`). The route is set as soon as routing matches, so every span created
by the handler (DB, HTTP, custom) carries `trooper.entry.route`. Exceptions are recorded as span events.

Already using `AddOpenTelemetry()`? Use `builder.Services.AddOpenTelemetry().UseTaskTrooper(o => ...).AddAspNetCore();`

## Quick start — worker service

```csharp
var builder = Host.CreateApplicationBuilder(args);
builder.Services.AddTaskTrooper(o => o.ServiceName = "billing-worker"); // DSN from TROOPER_DSN
builder.Services.AddHostedService<MyWorker>();
```

## Kafka (Confluent.Kafka)

```csharp
builder.Services.AddTaskTrooper().AddKafka();

// consumer: entry span "process <topic>" continuing the producer's trace
var result = consumer.Consume(ct);
await result.ProcessAsync("billing-workers", HandleAsync /*, processName: "invoice-writer", attempt: 2 */);
// on giving up: TrooperKafka.MarkDeadLettered();

// producer: producer span + traceparent/baggage headers (incl. tt.svc)
await producer.ProduceTracedAsync("orders.created", new Message<string, string> { Key = id, Value = json });
```

Consumer span attributes: `messaging.system=kafka`, `messaging.destination.name`, `messaging.consumer.group.name`,
`messaging.destination.partition.id`, `messaging.kafka.offset`, `trooper.process` (default: group id), `trooper.entry.route`,
`trooper.consumer.attempt` (argument, else `tt-attempt` header, else 1), `trooper.consumer.dlq`, `trooper.caller`.
Manual control: `using var span = TrooperKafka.StartConsume(result, groupId);`.

## RabbitMQ (RabbitMQ.Client 7)

```csharp
builder.Services.AddTaskTrooper().AddRabbitMQ(o => o.ProcessName = "billing.orders");

consumer.ReceivedAsync += async (_, ea) =>
{
    TrooperRabbitMQ.Enrich(ea, queue: "billing.orders"); // process, attempt (x-delivery-count / x-death), caller
    ...
};
```

RabbitMQ.Client's own spans are used (`RabbitMQ.Client.Publisher`/`.Subscriber`); published messages get W3C `baggage` with `tt.svc`.

## MassTransit, EF Core, Redis

```csharp
builder.Services.AddTaskTrooper()
    .AddMassTransit()            // receive span → trooper.process = queue name
    .AddEntityFrameworkCore()
    .AddRedis();                 // uses IConnectionMultiplexer from DI
```

Npgsql (8+), SqlClient and HttpClient (incl. gRPC clients) need nothing extra.

## AI / LLM monitoring (ADR-023, contracts §25)

LLM calls are exported as OTel GenAI client spans (`gen_ai.operation.name`, `gen_ai.provider.name`, models,
`gen_ai.usage.*`, `trooper.llm.ttft_ms`). `AddTaskTrooper()` listens by default (`EnableLlm`) to:

| Source | Library |
|---|---|
| `Experimental.Microsoft.Extensions.AI` (+ `Microsoft.Extensions.AI*`) | `Microsoft.Extensions.AI`: `chatClient.AsBuilder().UseOpenTelemetry().Build()` (also embedding generators) |
| `OpenAI.*` | OpenAI .NET SDK — the SDK turns on its `OpenAI.Experimental.EnableOpenTelemetry` AppContext switch (unless you set it) |
| `Azure.AI.Inference*`, `Microsoft.SemanticKernel*` | emitted when those libraries' own telemetry is enabled |

```csharp
IChatClient chat = new OpenAI.Chat.ChatClient("gpt-4o-mini", key).AsIChatClient()
    .AsBuilder().UseOpenTelemetry().Build();
```

Any other client — manual helper (`ActivitySource` `TaskTrooper.Telemetry.Llm`):

```csharp
using var call = TrooperLlm.Start("anthropic", "claude-sonnet-4-5");      // operation defaults to "chat"
await foreach (var chunk in stream) { call.FirstToken(); /* … */ }      // trooper.llm.ttft_ms
call.ResponseModel(model).FinishReasons("end_turn")
    .End(new LlmUsage(inputTokens, outputTokens, CacheReadInputTokens: cached));
// on error: call.Fail(exception);   content: call.Prompt(text) / Completion(text) / InputMessages(json)
```

**Content (prompts / completions)** — `LlmCaptureContent` / `TROOPER_LLM_CAPTURE_CONTENT`:
- `true`: `TrooperLlm` records content, and `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true` is set (when unset) —
  M.E.AI's default for `EnableSensitiveData`. M.E.AI reads that variable once per process, so with the env var it applies
  from `AddTaskTrooper()` on; if a telemetry chat client could be created earlier, set `EnableSensitiveData = true` in
  `UseOpenTelemetry(configure: …)`.
- `false`: content tags (`gen_ai.input.messages`, `gen_ai.output.messages`, `gen_ai.system_instructions`, tool call
  arguments/results, legacy `gen_ai.prompt*`/`gen_ai.completion*`) are removed from every GenAI span, even if a library
  recorded them.
- unset (default): nothing recorded by `TrooperLlm`; an explicit `EnableSensitiveData = true` in M.E.AI is respected.

`TrooperGenAIProcessor` also fills `gen_ai.provider.name` from the older `gen_ai.system` and converts M.E.AI's
`gen_ai.response.time_to_first_chunk` (seconds) to `trooper.llm.ttft_ms`.

## Configuration

Order (later wins): environment variables → `TaskTrooper` config section (`appsettings.json`) → code delegate.

| Option | Env | Default | Notes |
|---|---|---|---|
| `Dsn` | `TROOPER_DSN` | — | `https://<key>@<host>[:port]/<project>`. Without it nothing is exported (instrumentation still runs). Local: `http://tt_dev_ingest_key@localhost:4318/1` |
| `ServiceName` | `TROOPER_SERVICE`, `OTEL_SERVICE_NAME` | app name | `service.name` |
| `ServiceVersion` | `TROOPER_VERSION` | entry assembly informational version | `service.version` (deploy marker) |
| `Environment` | `TROOPER_ENVIRONMENT` | host environment, lower-case | `deployment.environment.name` |
| `SampleRate` | `TROOPER_SAMPLE_RATE` | `1.0` | Parent-based ratio sampling; entry spans get `trooper.sample_rate` |
| `Enabled` | `TROOPER_ENABLED` | `true` | Disables export |
| `EnableTraces` / `EnableMetrics` / `EnableLogs` | — | `true` | Per-signal export |
| `ExportTimeout` | — | 10 s | Per HTTP attempt |
| `MaxExportRetries` | — | 5 | 429/5xx/network: exponential backoff 1 s → 60 s with jitter, honours `Retry-After`; other 4xx drop the batch |
| `EnableLlm` | — | `true` | Listen to the GenAI ActivitySources above |
| `LlmCaptureContent` | `TROOPER_LLM_CAPTURE_CONTENT` | unset | `true` / `false` / unset, see AI / LLM monitoring |
| `ResourceAttributes`, `Sources`, `Meters` | — | — | Extra resource attributes / ActivitySources / Meters |

```json
{ "TaskTrooper": { "Dsn": "https://key@ingest.trooper.dev/1", "ServiceName": "orders-api", "SampleRate": 0.2 } }
```

## Wire & conventions

- OTLP/HTTP protobuf to `<dsn endpoint>/v1/traces`, `/v1/metrics` (every 30 s), `/v1/logs`; header `x-trooper-key: <key>`.
- Resource: `service.name`, `service.version`, `deployment.environment.name`, `telemetry.distro.name=trooper-dotnet`, `telemetry.distro.version`.
- Outgoing calls (HttpClient, Kafka/RabbitMQ helpers) send `traceparent` + `baggage: tt.svc=<service.name>`; the receiver writes
  it to `trooper.caller`.
- Privacy: no headers, bodies or query values are captured (ASP.NET Core / HttpClient query strings are redacted by OpenTelemetry).
- The SDK never throws into the host: internal errors are swallowed and counted in `trooper.sdk.errors` (meter `TaskTrooper.Telemetry`).

## Build & test

No local `dotnet` needed — `./build.sh` runs in `mcr.microsoft.com/dotnet/sdk:10.0` (NuGet cache in the `ttnuget` volume):

```bash
./build.sh          # restore + build (Release) + test
./build.sh build    # build only
./build.sh pack     # NuGet packages → artifacts/
```

Samples: `samples/MinimalApi` (ASP.NET Core) and `samples/KafkaWorker` (worker service consuming `orders.created`).

Continuous profiling is not available in the .NET SDK yet (contracts §24; Go / JVM / Node only).
