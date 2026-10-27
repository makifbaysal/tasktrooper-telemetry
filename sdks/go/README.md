# trooper-go — TaskTrooper Telemetry for Go

A thin [OpenTelemetry](https://opentelemetry.io/docs/languages/go/) distribution (ADR-011). `trooper.Start` configures the
global OTel tracer, meter and logger providers to export **OTLP/HTTP protobuf (gzip)** to the TaskTrooper ingest gateway,
and adds the `trooper.*` conventions from `.ai/contracts.md` §4. Spans you create with the plain OTel API are exported too.

```
go get github.com/tasktrooper/telemetry-go
```

## Quick start

```go
import trooper "github.com/tasktrooper/telemetry-go"

func main() {
	shutdown, err := trooper.Start(context.Background(), trooper.Config{
		DSN:         os.Getenv("TROOPER_DSN"), // http://tt_dev_ingest_key@localhost:4318/1
		Service:     "orders-api",
		Environment: "production",
		Version:     os.Getenv("GIT_SHA"),
	})
	if err != nil {
		log.Printf("telemetry disabled: %v", err) // the app keeps running
	}
	defer shutdown(context.Background()) // flushes buffered telemetry
	// ...
}
```

`Start` never panics; on error it returns a no-op shutdown function. Without a DSN (and without
`OTEL_EXPORTER_OTLP_ENDPOINT`) nothing is exported, but propagation still works.

## Configuration

| Field | Env (first wins) | Default | Notes |
|---|---|---|---|
| `DSN` | `TROOPER_DSN` | — | `https://<ingest-key>@<host>[:port]/<project>`. A set `OTEL_EXPORTER_OTLP_ENDPOINT` overrides its endpoint; without a DSN, `OTEL_EXPORTER_OTLP_HEADERS` supplies auth |
| `Service` | `OTEL_SERVICE_NAME`, `TROOPER_SERVICE` | `unknown_service:<exe>` | `service.name` |
| `Environment` | `TROOPER_ENV` | — | `deployment.environment.name` (also via `OTEL_RESOURCE_ATTRIBUTES`) |
| `Version` | `TROOPER_VERSION` | — | `service.version`, used as deploy marker |
| `SampleRate` | `OTEL_TRACES_SAMPLER_ARG`, `TROOPER_SAMPLE_RATE` | `1.0` | head sampling probability (parent-based ratio) |
| `Debug` | `TROOPER_DEBUG` | `false` | print every internal error to stderr (otherwise ≤ 1/min) |
| `Disabled` | `TROOPER_ENABLED=false` | `false` | install nothing; `Start` returns a no-op shutdown |
| `ResourceAttributes` | `OTEL_RESOURCE_ATTRIBUTES` | — | extra low-cardinality resource attributes |
| `DisableTraces/Metrics/Logs` | — | `false` | turn a signal off |
| `DisableRuntimeMetrics` | — | `false` | Go runtime metrics (`go.*`) |
| `MetricInterval` | — | `30s` | metric export interval |
| `BatchTimeout` | — | `5s` | max delay before a span/log batch is sent |
| `ExportTimeout` | — | `30s` | per export, including retries |
| `MaxQueueSize` | — | `2048` | bounded span/log queues; overflow is dropped, never blocks |
| `HTTPClient` | — | — | custom client for exporters (proxy, TLS) |
| `Profiling` / `trooper.WithProfiling(...)` | `TROOPER_PROFILING`, `TROOPER_PROFILING_TYPES`, `TROOPER_PROFILING_PERIOD`, `TROOPER_PROFILING_CPU_DURATION` | off | continuous profiling, see [below](#continuous-profiling) |
| — (`llm.SetCaptureContent`) | `TROOPER_LLM_CAPTURE_CONTENT` | `false` | record LLM prompts/completions, see [below](#ai--llm-monitoring) |

Explicit fields win; then explicitly set `OTEL_*` variables; then `TROOPER_*` (contract §2).

## What the distro does

- **Export:** `POST {endpoint}/v1/traces|metrics|logs`, protobuf + gzip, header `x-trooper-key`. Batch processors with bounded
  queues. Retries 429/5xx with exponential backoff (1s → 60s, jitter, honours `Retry-After`) within `ExportTimeout`; other 4xx
  drop the batch. Swallowed errors are counted in `trooper.sdk.errors` (meter `TaskTrooper.Telemetry`, attr `component`:
  `exporter.traces|metrics|logs|profiles`, `processor`, `capture`, `resolver`, `profiler`, `profiler.cpu`, `llm`), lost items in `trooper.sdk.dropped`; see also
  `trooper.DroppedBatches()` / `trooper.SDKErrors()`.
- **Resource:** `service.name/version`, `deployment.environment.name`, `telemetry.distro.name=trooper-go`, `telemetry.sdk.*`,
  host, OS, container id, process pid/executable/runtime (no command line or owner — they may contain secrets).
- **Sampling:** `ParentBased(TraceIDRatioBased(SampleRate))`. Root spans write the rate into W3C tracestate as
  `tt=r:<rate>`. Entry spans (server/consumer spans without a local parent) carry `trooper.sample_rate` — taken from the
  upstream `tt=r:` when the parent is remote (that rate decided the trace), else the configured rate — so the writer can
  scale RED counts by `1/sample_rate`. Error/slow tail sampling is done **server side**; keep `SampleRate=1` unless volume
  requires it.
- **Propagation:** W3C `traceparent` + `baggage`. Every outgoing injection sets baggage `tt.svc=<service>` (replacing the
  upstream value); server/consumer integrations record the incoming value as `trooper.caller`.
- **Entry route:** server/consumer integrations call `trooper.BeginEntry`; the trooper span processor copies
  `trooper.entry.route` and `trooper.entry.method` onto **every** span started under that request — including DB, HTTP client
  and producer spans created by other instrumentation — so the backend can build `edges_1m`.
- **Route templates:** `trooper.route` is the normalized template: `{id}` → `:id`, `{id:[0-9]+}` → `:id`, `{p...}` → `*p`.
- **PII:** no headers, bodies, query strings, message payloads/keys or SQL literals are recorded. SQL text is sanitized with
  `trooper.SanitizeSQL` (literals → `?`).

## API

```go
trooper.CaptureError(ctx, err)          // exception event + stack on the active span (or an ERROR log record)
trooper.RecordPanic(ctx, recovered)     // used by integrations' recover
trooper.MarkDLQ(ctx)                    // trooper.consumer.dlq=true on the consumer span
ctx, e := trooper.BeginEntry(ctx, method, route, resolver) // for custom servers/consumers; e.Finish(route)
ctx, span := trooper.StartConsumer(ctx, carrier, trooper.Message{...})   // custom consumers
err := trooper.ProcessMessage(ctx, carrier, msg, trooper.PanicRepanic, fn)
ctx, span := trooper.StartProducer(ctx, carrier, trooper.Message{...})
trooper.SetAttempt(carrier, n)          // tt-attempt header when re-publishing to a retry topic
trooper.NewSampler(rate), trooper.NewSpanProcessor(rate), trooper.NewPropagator(service) // bring-your-own TracerProvider
```

Panics: middlewares record the panic (exception event, status Error) and by default **re-panic** so the framework's own
recovery (`gin.Recovery`, echo `Recover`, `net/http`) responds. `WithPanicPolicy(trooper.PanicRecover)` answers 500 /
`codes.Internal` instead.

## Integrations

Each integration is a separate Go module so you only download the frameworks you use:
`go get github.com/tasktrooper/telemetry-go/integrations/<name>`.

| Module (`integrations/…`) | Package | Wraps |
|---|---|---|
| `gin` | `troopergin` | `otelgin` |
| `echo` | `trooperecho` | `labstack/echo-otel/v4` (replaces the deprecated `otelecho`) — echo v4 |
| `fiber` | `trooperfiber` | `gofiber/contrib/otelfiber/v2` — Fiber v2 |
| `fiberv3` | `trooperfiber3` | `gofiber/contrib/v3/otel` — Fiber v3 |
| `chi` | `trooperchi` | `otelhttp` (via `nethttp`) |
| `nethttp` | `trooperhttp` | `otelhttp` — server (`ServeMux` patterns via `r.Pattern`) and client transport |
| `grpc` | `troopergrpc` | `otelgrpc` stats handlers + panic interceptors |
| `franzgo` | `trooperfranz` | own kgo producer hooks + `ProcessRecord` |
| `sarama` | `troopersarama` | `IBM/sarama` consumer-group handler, `SendMessage`, interceptor |
| `amqp091` | `trooperamqp` | `rabbitmq/amqp091-go` `Publish` / `ProcessDelivery` |
| `kafkago` | `trooperkafkago` | `segmentio/kafka-go` `ProcessMessage` / `WriteMessages` |
| `sql` | `troopersql` | `XSAM/otelsql` |
| `pgx` | `trooperpgx` | `exaring/otelpgx` (pgx v5) |
| `goredis` | `trooperredis` | `redisotel` (go-redis v9) |
| `mongo` | `troopermongo` | own `event.CommandMonitor` (mongo-driver v2) |
| `slog` | `trooperslog` | `otelslog` + trace_id/span_id on your local handler |
| `zap` | `trooperzap` | `otelzap` |

Server middlewares set `trooper.route`, `http.route`, `trooper.entry.route/method` (server + all child spans),
`trooper.caller` and record panics. Options: `WithPanicPolicy(...)` plus pass-through of the wrapped contrib options.

### HTTP servers

```go
// gin
r := gin.New()
r.Use(gin.Recovery())
troopergin.Use(r)                       // or r.Use(troopergin.Middleware()...)

// echo v4
e := echo.New()
e.Use(middleware.Recover(), trooperecho.Middleware())

// Fiber v2 / v3
app := fiber.New()
trooperfiber.Use(app)                   // trooperfiber3.Use(app) for Fiber v3

// chi
r := chi.NewRouter()
r.Use(trooperchi.Middleware())

// net/http (Go 1.22+ patterns)
mux := http.NewServeMux()
mux.HandleFunc("GET /orders/{id}", getOrder)   // trooper.route = /orders/:id
http.ListenAndServe(":8080", trooperhttp.Handler(mux))

// gRPC
srv := grpc.NewServer(
	grpc.StatsHandler(troopergrpc.ServerHandler()),
	grpc.ChainUnaryInterceptor(troopergrpc.UnaryServerInterceptor()),
	grpc.ChainStreamInterceptor(troopergrpc.StreamServerInterceptor()),
)
conn, _ := grpc.NewClient(addr, grpc.WithStatsHandler(troopergrpc.ClientHandler()))
```

### HTTP client

```go
client := trooperhttp.NewClient(trooperhttp.WithPeerService("payments"))  // or trooperhttp.NewTransport(base, ...)
req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)                  // always pass the request ctx
```

### Messaging

```go
// franz-go
tt := trooperfranz.New(trooperfranz.WithConsumerGroup("billing"), trooperfranz.WithProcess("charge-order"))
cl, _ := kgo.NewClient(kgo.ConsumerGroup("billing"), kgo.ConsumeTopics("orders"), kgo.WithHooks(tt.Hooks()...))
fetches.EachRecord(func(rec *kgo.Record) {
	_ = tt.ProcessRecord(ctx, rec, func(ctx context.Context) error { return handle(ctx, rec) })
})

// sarama
h := troopersarama.NewConsumerGroupHandler(func(ctx context.Context, m *sarama.ConsumerMessage) error { ... },
	troopersarama.WithConsumerGroup("billing"))
_ = group.Consume(ctx, []string{"orders"}, h)
_, _, err := troopersarama.SendMessage(ctx, syncProducer, msg)

// RabbitMQ
err := trooperamqp.Publish(ctx, ch, "orders", "created", false, false, amqp.Publishing{Body: b})
for d := range deliveries {
	err := trooperamqp.ProcessDelivery(ctx, d, func(ctx context.Context) error {
		if poison { trooper.MarkDLQ(ctx); return d.Nack(false, false) }
		return d.Ack(false)
	}, trooperamqp.WithQueue("orders.created"))
}

// segmentio/kafka-go
err := trooperkafkago.ProcessMessage(ctx, msg, handle, trooperkafkago.WithConsumerGroup("billing"))
err = trooperkafkago.WriteMessages(ctx, writer, msgs...)
```

Consumer spans are entry spans: `messaging.system`, `messaging.destination.name`, `messaging.consumer.group.name`,
`messaging.destination.partition.id`, `trooper.process` (default: group, then destination), `trooper.consumer.attempt`
(header `tt-attempt`, set it with `trooper.SetAttempt` when re-publishing to a retry topic; `WithAttemptHeader` for
 a custom header; RabbitMQ also `x-delivery-count`/`x-death`/redelivered), `trooper.caller`, and the parent trace from headers.

### Databases

```go
db, err := troopersql.Open("pgx", dsn, troopersql.WithNamespace("orders"))   // db.system inferred from driver

cfg, _ := pgxpool.ParseConfig(dsn)
cfg.ConnConfig.Tracer = trooperpgx.NewTracer()

_ = trooperredis.Instrument(rdb)

client, _ := mongo.Connect(options.Client().ApplyURI(uri).SetMonitor(troopermongo.NewMonitor()))
```

### Logs

```go
logger := slog.New(trooperslog.NewHandler("orders-api", slog.NewJSONHandler(os.Stdout, nil)))
logger.InfoContext(ctx, "order created")   // → OTel logs + local JSON with trace_id/span_id

core := trooperzap.Tee(zapcore.NewCore(enc, os.Stdout, zap.InfoLevel), "orders-api")
logger := zap.New(core)
logger.Info("order created", trooperzap.Context(ctx))
```

### Notes and limits

- **Fiber:** the route is known only after routing, so child spans get `trooper.entry.route` lazily from `c.Route()`; spans
  started in middleware *before* the route handler get none. Fiber v2 uses `otelfiber` v2.2.3, which emits older HTTP semconv
  names. Response trace header injection is disabled (no baggage leak to clients).
- **net/http, chi:** the route is resolved lazily from `r.Pattern` / chi's route context while the request runs.
- **Profile labels:** see [Continuous profiling](#continuous-profiling) — Fiber requests are labelled with `trooper.method` only.
- **gRPC:** `trooper.route` = full method (`/pkg.Svc/Method`); there is no `trooper.entry.method`. Unknown methods produce no span.
- **sarama `ProducerInterceptor`:** has no context, so it only propagates `tt.svc`; use `SendMessage`/`InjectContext` for traces.
- **mongo:** no released contrib `otelmongo` exists for driver v2, so a small monitor is included; command text is off by
  default (`WithCommandText()` records it with all values replaced by `?`).
- **redis:** command arguments (`db.statement`) are not recorded.

Runnable examples: [`examples/gin`](examples/gin/main.go), [`examples/franzgo`](examples/franzgo/main.go).

## Continuous profiling

Off by default (ADR-022, contract §24). Enable in code or with `TROOPER_PROFILING=true`:

```go
shutdown, err := trooper.Start(ctx, trooper.Config{Service: "orders-api"}, trooper.WithProfiling())

// or choose types / timing:
trooper.Start(ctx, cfg, trooper.WithProfiling(
	trooper.WithProfileTypes("cpu", "alloc_space", "inuse_space", "goroutines", "lock", "block"),
	trooper.WithProfilePeriod(60*time.Second),
	trooper.WithProfileCPUDuration(10*time.Second),
))
```

| Env | Default | |
|---|---|---|
| `TROOPER_PROFILING` | `false` | `true` turns profiling on (code option wins) |
| `TROOPER_PROFILING_TYPES` | `cpu,alloc_space,inuse_space,goroutines` | also `alloc_objects`, `inuse_objects`, `lock` (mutex), `block` |
| `TROOPER_PROFILING_PERIOD` | `60` | seconds between collections |
| `TROOPER_PROFILING_CPU_DURATION` | `10` | seconds of CPU profiling per period (≤ period) |

How it works:

- A background goroutine runs `runtime/pprof` CPU profiling for `CPU_DURATION` every `PERIOD` (default 10 s / 60 s — well
  under the 2% CPU budget) and then snapshots the other profiles. Each profile is one `POST {endpoint}/v1/profiles` with
  `service, env, version, host, runtime=go, start, end, format` query parameters and the DSN key header.
- `cpu` and `goroutines` are sent as gzipped **pprof** (as written by `runtime/pprof`, labels included). Heap, mutex and block
  profiles are cumulative since process start in Go, so the SDK decodes them and sends the **delta per period** as
  **folded** stacks (`type`/`unit` parameters; `inuse_*` are point-in-time snapshots). The first period only records the
  baseline for `alloc_*`, `lock` and `block`.
- `lock` / `block` set `runtime.SetMutexProfileFraction(10)` (unless the application already set a fraction) and
  `runtime.SetBlockProfileRate(100µs)` only when requested; both are reset on shutdown.
- If CPU profiling is already active (your own `pprof.StartCPUProfile`, `net/http/pprof`'s `/debug/pprof/profile`), that
  round is skipped and counted in `trooper.sdk.errors{component="profiler.cpu"}`. Conversely, `/debug/pprof/profile` fails
  while the SDK's 10 s CPU window is running.
- Upload failures drop the profile (no queue). 429/5xx/network errors start an exponential backoff (1 s → 60 s, jitter,
  `Retry-After` honoured) during which profiles are dropped (`trooper.sdk.dropped{component="profiler"}`); other 4xx drop.
- Shutdown stops the loop; an in-flight CPU window is stopped and discarded.

**Endpoint labels.** While profiling runs, entry points label their goroutine with pprof labels `trooper.route` and
`trooper.method` (HTTP method, `GRPC`, `CONSUME`), so the flame graph can be filtered per endpoint. The returned context
carries the labels too; goroutines started from the request goroutine inherit them, and workers that receive the context
can use `pprof.SetGoroutineLabels(ctx)` / `pprof.Do`. Labels are restored when the request ends. When profiling is off no
labels are set (zero cost). `trooper.EnableProfileLabels(true)` turns them on for an external profiler.

| Integration | Route label |
|---|---|
| gin, echo, gRPC, consumers (`ProcessMessage`) | known when the middleware runs |
| net/http with `*http.ServeMux` | pre-resolved via `ServeMux.Handler(r)` (one extra match per request, only while profiling) |
| chi | pre-resolved via the router's `Match` on the chi route context |
| net/http with another router | `trooperhttp.WithProfileRouteResolver(func(*http.Request) string)`; otherwise method only |
| fiber | method only (the route is known only after the handler ran) |

Custom entry points: `trooper.BeginEntry` labels the goroutine when the route is passed; `entry.LabelRoute(ctx, route)` adds
it later. `Finish` must run on the same goroutine as `BeginEntry`. `trooper.StartConsumer` leaves the labels on the goroutine
until the next entry; `ProcessMessage` restores them.

## AI / LLM monitoring

Package `github.com/tasktrooper/telemetry-go/llm` (no extra dependencies) records LLM calls as OTel GenAI client spans
(ADR-023, contract §25): `gen_ai.operation.name`, `gen_ai.provider.name`, request/response model, `max_tokens`,
`temperature`, input/output and cache read/write tokens, `finish_reasons`, response id, `trooper.llm.ttft_ms` for streams.

**Zero-code: wrap the SDK's HTTP client.**

```go
import "github.com/tasktrooper/telemetry-go/llm"

oa := openai.NewClient(option.WithHTTPClient(llm.HTTPClient(nil)))            // openai-go
an := anthropic.NewClient(aoption.WithHTTPClient(llm.HTTPClient(nil)))        // anthropic-sdk-go
genai.NewClient(ctx, &genai.ClientConfig{HTTPClient: llm.HTTPClient(nil)})    // google.golang.org/genai

// existing client / transport, extra OpenAI-compatible hosts:
client := llm.HTTPClient(myClient, llm.WithProvider("localhost:11434", "ollama"))
rt := llm.Transport(otelhttp.NewTransport(http.DefaultTransport))             // LLM span becomes the parent of the HTTP span
```

| Host | `gen_ai.provider.name` | APIs |
|---|---|---|
| `api.openai.com` | `openai` | `/chat/completions`, `/completions`, `/embeddings`, `/responses` |
| `*.openai.azure.com`, `*.cognitiveservices.azure.com`, `*.services.ai.azure.com` | `azure.ai.openai` | same; model falls back to the deployment name |
| `api.anthropic.com` | `anthropic` | `/v1/messages` |
| `generativelanguage.googleapis.com`, `*aiplatform.googleapis.com` | `gcp.gemini`, `gcp.vertex_ai` | `:generateContent`, `:streamGenerateContent`, `:embedContent` |
| `api.mistral.ai`, `api.groq.com`, `api.deepseek.com`, `api.x.ai`, `openrouter.ai`, `api.together.xyz`, `api.fireworks.ai`, `api.perplexity.ai`, `api.cerebras.ai` | `mistral_ai`, `groq`, `deepseek`, `x_ai`, `openrouter`, … | OpenAI-compatible |
| `llm.WithProvider(host, name)` | `name` | OpenAI-compatible (Anthropic / Gemini dialect for `anthropic` / `gcp.gemini`) |

- The request body is buffered (≤ 8 MB) to read `model`, `max_tokens` / `max_completion_tokens` / `maxOutputTokens`,
  `temperature`, and replayed byte-for-byte. The response body is passed through unchanged while a copy is parsed: JSON
  `usage` (OpenAI `prompt_tokens`/`completion_tokens`/`prompt_tokens_details.cached_tokens`; Anthropic
  `input_tokens`/`output_tokens`/`cache_read_input_tokens`/`cache_creation_input_tokens`; Gemini `usageMetadata`), and
  SSE streams event by event — TTFT at the first content delta, the span ends at EOF or `Close`.
- `gen_ai.usage.input_tokens` includes cached tokens (OTel convention): for Anthropic it is
  `input_tokens + cache_read_input_tokens + cache_creation_input_tokens`. Gemini output tokens include thinking tokens.
- OpenAI chat streams report usage only with `stream_options: {"include_usage": true}` (the transport never changes your
  request); without it the span has TTFT and model but no tokens.
- Non-2xx responses: span status Error, `error.type` = status code. Network errors: `error.type` = error type.
- Unknown hosts, other paths and non-POST requests pass through untouched; a parsing failure only loses telemetry.

**Manual instrumentation** (other SDKs, Bedrock, agents, tools):

```go
ctx, call := llm.Start(ctx, llm.Request{Provider: "aws.bedrock", Model: modelID, Operation: "chat", MaxTokens: 1024})
for chunk := range stream(ctx) {
	call.FirstToken() // TTFT: only the first call counts
	...
}
call.End(llm.Usage{InputTokens: in, OutputTokens: out, FinishReasons: []string{"end_turn"}, Err: err})
```

**Content capture** is off by default (PII). `TROOPER_LLM_CAPTURE_CONTENT=true`, `llm.SetCaptureContent(true)` or
`llm.WithCaptureContent(true)` (per transport) record `gen_ai.system_instructions`, `gen_ai.input.messages` and
`gen_ai.output.messages` as JSON in the GenAI message format (`[{"role":"user","parts":[{"type":"text","content":"…"}]}]`);
with `llm.Start` pass `Request.SystemInstructions` / `InputMessages` and `Usage.OutputMessages`. Parts longer than 16 KB
are truncated; the backend stores content separately for 7 days.

## Development

```
cd sdks/go
scripts/test-all.sh            # build + vet + test every module (GOWORK=off)
scripts/test-all.sh -race
go test -tags e2e -run E2E .   # against a running ingest-gw on localhost:4318 (skips otherwise)
```

`go.work` ties the modules together for editors; each integration's `go.mod` also has `replace` directives to the local
core so modules build stand-alone. Remove the `replace` lines when publishing tagged versions.

Testing your own instrumentation: `troopertest.New(t, "svc")` installs an in-memory recorder with the trooper processor and
propagator.
