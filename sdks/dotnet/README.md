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
| `TaskTrooper.Telemetry.Profiling` | Continuous profiler: in-process EventPipe → `cpu` / `wall` / `alloc_space` / `lock`, per-endpoint flame graphs |

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

## Continuous profiling (ADR-022, contracts §24)

```csharp
builder.Services.AddTaskTrooper(o => o.ServiceName = "orders-api").AddAspNetCore();
builder.Services.AddTaskTrooperProfiling();                       // or .AddTaskTrooper(...).AddProfiling()
// options: o => { o.Types.Add(ProfileType.Wall); o.Period = TimeSpan.FromSeconds(60); o.Duration = TimeSpan.FromSeconds(10); }
```

Without code changes: reference the `TaskTrooper.Telemetry.Profiling` package and set `TROOPER_PROFILING=true` —
`AddTaskTrooper()` finds the package by name and registers its hosted service (it stays off unless enabled).

How it works:
- Every `Period` (default 60 s) the profiler opens an **in-process EventPipe session** (`DiagnosticsClient` on its own PID) for
  `Duration` (default 10 s) and streams it through TraceEvent's real-time `TraceLog` (a short rundown at the start names
  methods compiled earlier; JIT events name the rest). Profiles are uploaded as **folded** text, one per type, to
  `POST /v1/profiles?format=folded&type=…&unit=…&service=…&env=…&version=…&host=…&runtime=dotnet&start=…&end=…`
  (gzip, `x-trooper-key`); `start`/`end` are the window.
- Why windows instead of a continuous stream: EventPipe's sample profiler suspends the runtime and walks every managed
  thread at ~1 kHz (the rate is not configurable), which costs a few percent while it runs; 10 s out of 60 s keeps the average
  well within the < 2 % budget. `Duration = Period` profiles continuously.
- Types (`TROOPER_PROFILING_TYPES`, default `cpu,alloc_space,lock`):

  | Type | Unit | Source |
  |---|---|---|
  | `cpu` | ns | `Microsoft-DotNETCore-SampleProfiler` samples of threads running **managed** code, each weighted by the time since that thread's previous sample (≤ 10 ms). Native / blocked time is not CPU here. |
  | `wall` | ns | All thread samples (running or waiting). Opt-in. |
  | `alloc_space` | bytes | `GCAllocationTick` (≈ every 100 KB per heap) weighted by the allocated amount — a sampled view of allocations in the window. |
  | `lock` | ns | Monitor / `lock` contention: stack of `ContentionStart`, duration of `ContentionStop`. |

- Frames are `Namespace.Type.Method` (no signature / IL offset / generic instantiation; `Foo+<GetAsync>d__3.MoveNext` →
  `Foo.GetAsync`); frames without method info are `[module]` / `[native]`; stacks deeper than 128 start with `[truncated]`;
  more than 10 000 distinct stacks per profile fold into `[other]`. The profiler's own threads are excluded.
- **Endpoint attribution:** entry spans (local-root server / consumer spans — ASP.NET Core requests, Kafka/RabbitMQ consumers)
  mark their ExecutionContext, which follows the request across `await`s and thread-pool hops; samples taken on a thread
  while it runs a request's code are written under `# trooper.route=<route> trooper.method=<METHOD>`. Only sampled
  (recorded) requests carry a route; work outside requests is unattributed.
- Never crashes the host: if EventPipe is unavailable (`DOTNET_EnableDiagnostics=0` / `DOTNET_EnableDiagnostics_IPC=0`,
  read-only or missing `TMPDIR` for the IPC socket, browser / mobile), no DSN, or `TROOPER_ENABLED=false`, the profiler logs
  once and stays off. Errors are counted in `trooper.sdk.errors` (`component` = `profiler`, `exporter.profiles`). A failed
  upload drops that profile; 429 / 5xx / network errors pause uploads with exponential backoff (1 s → 60 s, `Retry-After`).
- Measured overhead (`samples/ProfilingOverhead`: 2 workers, ~7 000 traced requests/s with async hops, allocation and a
  shared lock; Linux container, 11 cores): **while a window runs** ≈ +10–13 % process CPU and −5…−10 % throughput (EventPipe
  suspensions at 1 kHz + event parsing on the profiler thread); averaged over the default 10 s / 60 s duty cycle ≈ +2 %.
  Request-light services sit well below that; raise `Period` or lower `Duration` to trade resolution for overhead.

| Option | Env | Default |
|---|---|---|
| `Enabled` | `TROOPER_PROFILING` | on after `AddTaskTrooperProfiling()`, else off |
| `Types` | `TROOPER_PROFILING_TYPES` | `cpu,alloc_space,lock` (`wall` opt-in; other types are not supported on .NET) |
| `Period` | `TROOPER_PROFILING_PERIOD` (s) | 60 s |
| `Duration` | `TROOPER_PROFILING_CPU_DURATION` (s) | 10 s (capped at `Period`) |
| `StartDelay` | — | 5 s |
| `Host` | — | machine name |
| `BufferSizeMb` | — | 64 (EventPipe drops events instead of blocking when full) |

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

Samples: `samples/MinimalApi` (ASP.NET Core), `samples/KafkaWorker` (worker service consuming `orders.created`) and
`samples/ProfilingOverhead` (profiler overhead check: `dotnet run -c Release --project samples/ProfilingOverhead -- 30 4`).
