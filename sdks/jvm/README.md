# TaskTrooper Telemetry — JVM SDKs (Java, Kotlin)

Thin OpenTelemetry distributions (ADR-011). All modules send OTLP/HTTP protobuf to `ingest-gw` with the
`x-trooper-key` header, and add the `trooper.*` attributes from `.ai/contracts.md` §4.

| Module | Use it when |
|---|---|
| `trooper-javaagent` → `trooper-javaagent.jar` | Any JVM app, no code change (Spring, Quarkus JVM mode, Ktor, Kafka, RabbitMQ, JDBC ... via the upstream agent) |
| `trooper-otel-extension` | You already ship the upstream OTel agent: `-Dotel.javaagent.extensions=trooper-otel-extension.jar` |
| `trooper-spring-boot-starter` | Spring Boot 3 without an agent (built on `opentelemetry-spring-boot-starter`) |
| `trooper-quarkus` | Quarkus 3 with `quarkus-opentelemetry` (JVM and native) |
| `trooper-ktor` | Ktor 3 server/client plugins (built on `opentelemetry-ktor-3.0`) |
| `trooper-kotlin` | Plain Kotlin/Java: `Trooper.init {}`, `withSpan {}`, `captureError()` |
| `trooper-otel-core` | Shared: DSN parsing, span processor, sampler, propagator, JFR profiler, GenAI span normalizer (used by all of the above) |
| `trooper-llm` | LLM monitoring helpers (OTel API only): `TrooperLlm.start(…)`, Anthropic Java SDK wrapper, LangChain4j listener |

Group `io.tasktrooper`, Java 17 bytecode. Build: `./gradlew build` (JDK 21; tests included).

## Quick start

### Java agent (zero code)

```bash
./gradlew :trooper-javaagent:trooperJavaagentJar        # → trooper-javaagent/build/libs/trooper-javaagent.jar
export TROOPER_DSN=https://<ingest-key>@ingest.trooper.dev/<project>
export TROOPER_SERVICE=orders-api TROOPER_ENV=production TROOPER_VERSION=$GIT_SHA
java -javaagent:trooper-javaagent.jar -jar app.jar
```

The jar is the upstream `opentelemetry-javaagent` with our extension embedded under `extensions/` (the documented distro
pattern). All `otel.*` agent settings still work and override the TaskTrooper defaults.

### Spring Boot 3 (no agent)

```kotlin
implementation("io.tasktrooper:trooper-spring-boot-starter:0.1.0")
```

```yaml
spring.application.name: orders-api   # used as service name if trooper.service is not set
trooper:
  dsn: ${TROOPER_DSN}
  environment: production
  sample-rate: 0.2
```

Spring MVC and WebFlux routes become `trooper.route` (`/v1/orders/{id}` → `/v1/orders/:id`); `@KafkaListener` /
`@RabbitListener` invocations set `trooper.process` (listener `id`, else `groupId`/queue) and `trooper.consumer.attempt`
(Spring Kafka `kafka_deliveryAttempt` header, or Spring Retry's retry count).
Micrometer: the OTel starter exports OTel metrics only. To ship existing Micrometer meters, add
`io.opentelemetry.instrumentation:opentelemetry-micrometer-1.5` and register `OpenTelemetryMeterRegistry` as a bean;
they then go through the same OTLP exporter.

### Quarkus 3

```kotlin
implementation("io.quarkus:quarkus-opentelemetry")
implementation("io.tasktrooper:trooper-quarkus:0.1.0")
```

```properties
quarkus.application.name=inventory-api
trooper.dsn=${TROOPER_DSN}
```

A `ConfigSourceFactory` maps `trooper.*` → `quarkus.otel.exporter.otlp.*`, service, resource and sampler (ordinal 150, so
explicit `quarkus.otel.*` wins). CDI beans add the span processor and sampler; a JAX-RS `ContainerRequestFilter` sets
`trooper.route` from the `@Path` template; propagators become `tracecontext,trooper` (W3C baggage + `tt.svc`).
SmallRye Reactive Messaging Kafka / RabbitMQ consumer spans get `trooper.consumer.attempt` automatically: the `trooper`
propagator reads the attempt while the connector extracts the trace context (`tt-attempt` → `kafka_deliveryAttempt` →
`x-delivery-count`+1 → `x-death`+1 → SmallRye `delayed-retry-count`+1, same order as the Spring starter) and the span
processor writes it on the consumer span. This requires the `trooper` propagator to be active (the default mapping;
if you set `quarkus.otel.propagators` yourself, keep `trooper` in the list).

### Ktor 3

```kotlin
implementation("io.tasktrooper:trooper-ktor:0.1.0")

fun Application.module() {
    install(TaskTrooper) {
        dsn = System.getenv("TROOPER_DSN")
        service = "users-api"
    }
    routing { get("/v1/users/{id}") { withSpan("load-user") { /* ... */ }; call.respondText("ok") } }
}

val http = HttpClient(CIO) {
    install(KtorClientTelemetry) { setOpenTelemetry(Trooper.openTelemetry) }   // client spans + traceparent
    install(TaskTrooperClient) { service = "users-api" }                      // baggage tt.svc
}
```

`TaskTrooper` initializes the SDK (or uses `openTelemetry = ...`, e.g. under the agent), installs `KtorServerTelemetry`
and sets `trooper.route` from the Ktor route as soon as routing resolves it. Context follows coroutines via
`asContextElement()`.

### Plain Kotlin / Java

```kotlin
Trooper.init { dsn = System.getenv("TROOPER_DSN"); service = "billing-worker" }

suspend fun handle(job: Job) = withSpan("handle-job") {
    try { process(job) } catch (e: Exception) { trooper.captureError(e) }
}
launch(Trooper.contextElement()) { /* keeps the current trace */ }
```

Java: `TrooperSdk.init(TrooperConfig.fromSystem())`; consumers: `TrooperConsumer.markCurrent("invoice-worker", attempt, dlq)`.

## Configuration

| Property (`-D` / `application.yml`) | Env var | Default | Effect |
|---|---|---|---|
| `trooper.dsn` | `TROOPER_DSN` | – | `https://<key>@<host>[:port]/<project>` → `otel.exporter.otlp.endpoint`, `protocol=http/protobuf`, header `x-trooper-key` |
| `trooper.service` | `TROOPER_SERVICE` | `spring.application.name` / `quarkus.application.name` | `service.name`; also sent as baggage `tt.svc` |
| `trooper.env` (alias `trooper.environment`) | `TROOPER_ENV` (alias `TROOPER_ENVIRONMENT`) | – (Spring: single active profile) | `deployment.environment.name` |
| `trooper.version` | `TROOPER_VERSION` | – | `service.version` (deploy marker) |
| `trooper.sample-rate` | `TROOPER_SAMPLE_RATE` | `1.0` | `parentbased_traceidratio`; entry spans get `trooper.sample_rate` |
| `trooper.enabled` | `TROOPER_ENABLED` | `true` | `false` → `otel.sdk.disabled=true` |

| `trooper.profiling.enabled` (alias `trooper.profiling`) | `TROOPER_PROFILING` | `false` | Continuous JFR profiler (below) |
| `trooper.profiling.types` | `TROOPER_PROFILING_TYPES` | `cpu,alloc_space,lock` | Subset of `cpu`, `alloc_space`, `lock`; other contract types (`inuse_space`, `wall` …) are ignored on the JVM |
| `trooper.profiling.period` | `TROOPER_PROFILING_PERIOD` | `60` | Seconds per uploaded profile (5–3600) |
| `trooper.llm.capture-content` | `TROOPER_LLM_CAPTURE_CONTENT` | unset | `true`: record prompts/completions (also sets `otel.instrumentation.genai.capture-message-content=true`); `false`: strip them from every GenAI span; unset: each library's own opt-in (all off by default) |

These are defaults only: any explicit `otel.*` / `quarkus.otel.*` setting wins. An invalid DSN is logged and ignored;
SDK errors never propagate into the application. Exporters retry 429/502/503/504 with exponential backoff 1s → 60s
(jittered, max 5 attempts), per contracts.md §2. Headers, bodies and query parameters are not captured (OTel defaults).

## What gets added

| Attribute | Where | Source |
|---|---|---|
| `trooper.route` | server spans | `http.route` / Spring MVC pattern / JAX-RS `@Path` / Ktor route, normalized (below); gRPC: `/pkg.Service/Method` |
| `trooper.entry.route` | every span of a request | the local root span's route (consumer: process name); read at child start and end |
| `trooper.entry.method` | every span with `trooper.entry.route` | entry HTTP method (`GET` …); `CONSUME` for consumers, `GRPC` for gRPC |
| `trooper.caller` | entry server spans | W3C baggage `tt.svc`; outgoing calls set `tt.svc=<service>` (baggage propagator wrapper) |
| `trooper.process` | consumer spans | explicit (listener id) → `messaging.consumer.group.name` → `messaging.destination.name` |
| `trooper.consumer.attempt` | consumer spans | `tt-attempt` header → `kafka_deliveryAttempt` → RabbitMQ `x-delivery-count`+1 (agent: captured via `otel.instrumentation.messaging.experimental.capture-headers`; starter: also `x-death`, Spring Retry; Quarkus: also `x-death`, SmallRye `delayed-retry-count`) |
| `trooper.sample_rate` | sampled entry spans (server/consumer, remote or no parent) | remote parent's tracestate `tt=r:<rate>`, else the configured ratio |

**Sampling rate propagation:** every root span writes its ratio to the W3C `tracestate` as `tt=r:<rate>` (e.g. `tt=r:0.25`);
downstream entry spans read it so the writer scales RED counts by the rate that actually decided (parent-based sampling).

**Route normalization** (contracts.md §4, same in all SDKs): `{id}`, `{id:regex}`, `{id?}`, `<id>`, `<int:id>`, `:id` → `:id`;
catch-alls `{p...}`, `{*p}`, `*p` → `*p`; raw number / UUID / hex / ULID / opaque-token segments → `:id`; trailing slash dropped.

**Retries across topics (`tt-attempt`):** when a listener re-publishes to a retry topic/queue, write the next attempt so the
next consumer reports it: starter `TrooperRetryHeaders.setNextAttempt(producerRecord.headers())` /
`rabbitTemplate.convertAndSend(q, msg, TrooperRetryHeaders::setNextAttemptAmqp)` (inside a listener the current attempt is
known), or generically `TrooperConsumer.writeAttempt((k, v) -> headers.add(k, v.getBytes(UTF_8)), attempt)`.

**SDK errors:** errors swallowed by the SDK are counted in `trooper.sdk.errors` (meter `TaskTrooper.Telemetry`, attribute
`component`, e.g. `span_processor`, `sampler`, `spring_listener`), exported with the app's other OTel metrics.

## Continuous profiling (ADR-022, contracts.md §24)

```bash
java -javaagent:trooper-javaagent.jar -Dtrooper.profiling.enabled=true -jar app.jar   # or TROOPER_PROFILING=true
```

Spring Boot starter: `trooper.profiling.enabled: true`; Quarkus (JVM mode): `trooper.profiling.enabled=true`;
Kotlin/Ktor: `Trooper.init { profiling = true }` or the env var. JDK 17+, Java Flight Recorder only — no native agent.

- A daemon thread runs a continuous JFR `RecordingStream`: `jdk.ExecutionSample` every 20 ms → `cpu` (value = samples ×
  20 ms in ns), `jdk.ObjectAllocationSample` (throttled 100/s) → `alloc_space` (sample weight, bytes),
  `jdk.JavaMonitorEnter` + `jdk.ThreadPark` ≥ 10 ms → `lock` (wait ns; parks only while serving a request, idle pool
  threads would dominate otherwise). JFR starts 5 s after the SDK so it stays out of application startup.
- Every period the stacks are folded (root first, frames `com.acme.Foo.bar`, ≤ 128 deep with `[truncated]` on the root
  side, generated lambda/proxy/CGLIB names made stable, ≤ 10 000 stacks per profile, the rest as `[other]`) and sent as
  `POST /v1/profiles?format=folded&type=…&unit=…&service=…&env=…&version=…&host=…&runtime=jvm&start=…&end=…`,
  gzip, `x-trooper-key` (DSN; without a DSN the OTLP/HTTP endpoint + headers are used). Metadata comes from the OTel
  resource. Failed uploads are dropped; 429/5xx/network errors pause uploads with backoff 1 s → 60 s (`Retry-After`).
  Uploads are not traced (OTel instrumentation suppression).
- **Endpoint attribution:** the span processor records, per thread, when it was working for an entry span (local-root
  SERVER/CONSUMER; child spans on other threads count too). Samples falling into such an interval go under
  `# trooper.route=<route> trooper.method=<METHOD>` (consumers: process name + `CONSUME`); the route is resolved when
  the period is flushed, so routes known only after routing are labelled correctly.
- When JFR is unavailable (JRE without `jdk.jfr`, native image, `-XX:-FlightRecorder`) the profiler logs once and stays off.

## AI / LLM monitoring (ADR-023, contracts.md §25)

LLM calls are OTel GenAI client spans (`gen_ai.operation.name`, `gen_ai.provider.name`, models, `gen_ai.usage.*`,
`trooper.llm.ttft_ms`). Prompts/completions only with `TROOPER_LLM_CAPTURE_CONTENT=true`.

- **OpenAI Java SDK**: instrumented by the javaagent (upstream `openai-java-1.1`). Without the agent, add
  `io.opentelemetry.instrumentation:opentelemetry-openai-java-1.1` and call `TrooperLlm.wrap(openAIClient)`.
- **Anthropic Java SDK** (`trooper-llm`, no dependency on the SDK — dynamic proxies + reflection; tested with 2.71, 3.x keeps the same accessors):
  ```java
  AnthropicClient client = TrooperLlm.wrap(AnthropicOkHttpClient.fromEnv());
  ```
  `messages().create(…)` / `createStreaming(…)` → `chat` spans with request/response model, `max_tokens`, input/output and
  cache read/creation tokens, stop reason, TTFT for streams; errors recorded and rethrown unchanged. `beta()` and
  `async()` services are not wrapped.
- **Spring AI**: its Micrometer observations reach OTel through the tracing bridge; the SDK normalizes them on export
  (`gen_ai.system` → `gen_ai.provider.name` with Spring AI values mapped, string token counts → numbers, finish reasons →
  array, ChatClient/advisor/vector-store observations are not counted as LLM calls, tool calls → `execute_tool`).
- **LangChain4j** (`trooper-llm`): `OpenAiChatModel.builder()…listeners(List.of(new TrooperChatModelListener()))`.
  Quarkus LangChain4j already emits GenAI spans.
- **Anything else**:
  ```java
  LlmCall call = TrooperLlm.start("anthropic", "claude-sonnet-4-5", "chat");
  try (Scope s = call.makeCurrent()) {
      for (var chunk : stream) { call.firstToken(); /* … */ }
      call.responseModel(m).finishReasons("end_turn").end(LlmUsage.of(in, out).withCache(cacheRead, cacheWrite));
  } catch (RuntimeException e) { call.fail(e); throw e; }
  ```
  `call.prompt(text)` / `completion(text)` / `inputMessages(json)` record content only when capture is on.

`trooper-llm` depends on `opentelemetry-api` only, so it works next to the javaagent (bridged), with the Spring Boot
starter (included) and with `TrooperSdk`.

## Limitations

- The span processor uses the SDK's `ExtendedSpanProcessor.onEnding` (internal API, stable in practice) to set routes
  resolved after span start.
- Agent: `trooper.consumer.attempt` comes from captured `tt-attempt`, Spring Kafka `kafka_deliveryAttempt` (needs
  `deliveryAttemptHeader=true`) or RabbitMQ `x-delivery-count`; `x-death` and Spring Retry are only visible in the starter.
  Quarkus reads them (plus `x-death` and SmallRye `delayed-retry-count`) through the `trooper` propagator.
  `trooper.consumer.dlq` is set via `TrooperConsumer.markCurrent(...)`.
- Quarkus: plain library, not a full extension (no deployment module); SmallRye Reactive Messaging consumers get
  `trooper.process` from the consumer group / destination, not the channel name. Not tested with `@QuarkusTest`
  (needs the Quarkus Gradle plugin build); covered by unit tests.
- The propagator wrapper only injects `tt.svc` when a baggage propagator is configured (default `tracecontext,baggage`).
- Profiling: no `inuse_space` / `wall` on the JVM; JFR's execution sampler takes at most a few threads per period and
  its density varies by platform (sparse on macOS), so `cpu` is a sample, not exact CPU time. Request attribution is
  per thread, so purely reactive code that hops threads without spans is attributed only where spans start.
- GenAI normalization and content stripping run on the exporter (javaagent, Spring Boot starter, `TrooperSdk`), not in
  Quarkus, where spans are exported as emitted.

## Layout / tests

`examples/` (`spring-boot-app`, `quarkus-app`, `ktor-app`) compile as part of the build and are not published.
Tests: core (DSN, config mapping, processor, sampler, propagator, real OTLP export to a local HTTP server, folded
aggregation, profile upload, route sections, real JFR profiling, GenAI normalization), `trooper-llm` (manual API, real
Anthropic SDK client incl. streaming against a fake API, LangChain4j listener), extension
(SPI via autoconfigure), javaagent (runs a tiny app with `-javaagent:trooper-javaagent.jar` against a fake ingest-gw
and asserts the protobuf payload; a second app with `TROOPER_PROFILING=true` asserts route-labelled folded profiles), Spring (MockMvc + WebTestClient), Quarkus (config source, sampler, JAX-RS template, consumer attempt from Kafka/RabbitMQ headers),
Ktor (`testApplication`), Kotlin helpers.
