# JVM SDK — testing

Cross-cutting test rules: [`.ai/testing.md`](../../../.ai/testing.md). Behaviour under test is specified in
[`.ai/sdk.md`](../../../.ai/sdk.md) and [`.ai/contracts.md`](../../../.ai/contracts.md).

## Gates — be honest about them

- **Not in PR CI, not in `make check`.** `.github/workflows/ci.yml` and the root `Makefile` cover Go services,
  `apps/web` and `sdks/js` only. Nothing runs the JVM tests on a PR.
- **Only gate:** [`.github/workflows/release-jvm.yml`](../../../.github/workflows/release-jvm.yml) on a `jvm-vX.Y.Z` tag
  runs `./gradlew build --no-configuration-cache` (compile + all tests + javadoc) on Temurin 21 before publishing.
  A red test blocks the release, which is late. So:
- **Run `./gradlew build` locally before every PR that touches `sdks/jvm`.** Say in the PR that you did.

## Commands

```bash
cd sdks/jvm
./gradlew build                               # everything: compile, tests, javadoc, examples
./gradlew test                                # all module tests
./gradlew :trooper-otel-core:test             # one module
./gradlew :trooper-otel-core:test --tests '*RouteTemplatesTest'
./gradlew :trooper-javaagent:test             # builds trooper-javaagent.jar, then runs the agent integration test
./gradlew build publishToMavenLocal           # release dry run → ~/.m2/repository/ai/tasktrooper/
```

Only failed tests are printed (`testLogging { events("failed") }`, full stack traces).

## Stack

- **JUnit 5 (Jupiter) + AssertJ**, added to every Java/Kotlin module by the root `build.gradle.kts`
  (`junit-bom`, `junit-jupiter`, `assertj-core`). Don't add them per module.
- Kotlin tests use the same JUnit 5 + AssertJ (no Kotest, no kotlin.test); coroutines via `kotlinx-coroutines-test`
  / `runBlocking`.
- **No mocking library** (no Mockito/MockK). Use real OTel objects and fakes:
  - `InMemorySpanExporter` / `opentelemetry-sdk-testing` with a real `SdkTracerProvider` (see `TrooperKotlinTest`,
    `LlmTestSupport`).
  - JDK `com.sun.net.httpserver.HttpServer` on `127.0.0.1:0` as a fake ingest-gw or fake upstream API
    (`JavaagentIntegrationTest`, `ExporterConfigTest`, `AnthropicWrapTest`).
  - Framework test harnesses: Spring `MockMvc` / `WebTestClient` (`SpringMvcTest`, `SpringWebFluxTest`), Ktor
    `testApplication` (`TaskTrooperKtorTest`).
- Quarkus is unit-tested without `@QuarkusTest` (needs the Quarkus Gradle plugin): test config source, propagator and
  filters directly (`QuarkusMappingTest`, `QuarkusAttemptTest`).

## Layout and style

- Tests mirror the main package: `src/test/java/io/tasktrooper/telemetry/<module>/…Test.java`
  (`src/test/kotlin/...` for Kotlin modules). Class name `<Subject>Test`, package-private.
- Table-driven where the input is a table: `@ParameterizedTest` + `@CsvSource(delimiter = '|')`
  (see `RouteTemplatesTest`). Route/DSN tables must match the shared contract tables used by the other SDKs.
- Assert on **what is exported** (span attributes, OTLP protobuf, `trooper.sdk.errors` counts), not on internals.
- Global state: `SdkErrors` / `SdkHeartbeat` have package-private `resetForTest()`; call it in `@BeforeEach`/`@AfterEach`
  when a test reads counts (see `TrooperSpanProcessorTest`). Build an explicit `OpenTelemetrySdk` per test instead of
  touching `GlobalOpenTelemetry`.
- No external network, no Docker, no real Kafka/Rabbit: bind to `127.0.0.1:0`, use header maps / test hosts.
- Keep timing tolerant (real JFR profiling is sparse on macOS); poll with a deadline instead of fixed sleeps.

## The javaagent integration test

`trooper-javaagent` has a separate `testApp` source set (`TinyApp`, `ProfiledApp`). The test task depends on the built
agent jar and passes `trooper.agentJar` / `trooper.testAppClasspath` system properties; `JavaagentIntegrationTest`
launches a child JVM with `-javaagent:` against a fake ingest server and asserts the decoded OTLP protobuf. Changes to
the extension, core SPI wiring or jar layout must keep this green.

## What every change needs

| Change | Test |
|---|---|
| New/changed `trooper.*` key or DSN rule | `DsnAndConfigTest` case |
| Route normalization | row in `RouteTemplatesTest` (and the same row in the other SDKs' tables) |
| New interceptor / processor / plugin | happy path **and** a failure path proving the host call still succeeds and `SdkErrors.count("<component>")` went up |
| Exporter / retry / payload change | test against a local `HttpServer` asserting the request |
| Framework integration | test through the framework's own test harness |
| GenAI / LLM | `GenAiSpansTest` / `trooper-llm` tests incl. content stripped when capture is off |
