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

Explicit fields win; then explicitly set `OTEL_*` variables; then `TROOPER_*` (contract §2).

## What the distro does

- **Export:** `POST {endpoint}/v1/traces|metrics|logs`, protobuf + gzip, header `x-trooper-key`. Batch processors with bounded
  queues. Retries 429/5xx with exponential backoff (1s → 60s, jitter, honours `Retry-After`) within `ExportTimeout`; other 4xx
  drop the batch. Swallowed errors are counted in `trooper.sdk.errors` (meter `TaskTrooper.Telemetry`, attr `component`:
  `exporter.traces|metrics|logs`, `processor`, `capture`, `resolver`), lost items in `trooper.sdk.dropped`; see also
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
- **gRPC:** `trooper.route` = full method (`/pkg.Svc/Method`); there is no `trooper.entry.method`. Unknown methods produce no span.
- **sarama `ProducerInterceptor`:** has no context, so it only propagates `tt.svc`; use `SendMessage`/`InjectContext` for traces.
- **mongo:** no released contrib `otelmongo` exists for driver v2, so a small monitor is included; command text is off by
  default (`WithCommandText()` records it with all values replaced by `?`).
- **redis:** command arguments (`db.statement`) are not recorded.

Runnable examples: [`examples/gin`](examples/gin/main.go), [`examples/franzgo`](examples/franzgo/main.go).

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
