# JVM SDK — coding standards

Scope: `sdks/jvm` (Gradle Kotlin DSL multi-project). Cross-cutting rules live in the repo root:
[`.ai/coding-standards.md`](../../../.ai/coding-standards.md), SDK design in [`.ai/sdk.md`](../../../.ai/sdk.md),
wire/attribute/env contracts in [`.ai/contracts.md`](../../../.ai/contracts.md). This file only adds what is JVM-specific.

## Toolchain and build

- **Gradle 9.6** via the wrapper (`./gradlew`); never a system Gradle. Build logic is Kotlin DSL only (`*.gradle.kts`).
- **Bytecode target is Java 17** for every module: `options.release = 17` (Java) and `jvmTarget = JVM_17` +
  `-Xjdk-release=17` (Kotlin), set once in the root [`build.gradle.kts`](../build.gradle.kts). Don't override per module.
  The build JDK can be newer (release CI uses Temurin 21); don't use APIs newer than 17.
- **Versions live only in [`gradle/libs.versions.toml`](../gradle/libs.versions.toml).** Modules reference `libs.*` and
  BOMs (`platform(libs.otel.bom)`, `libs.otel.instrumentation.bom.alpha`, `libs.spring.boot.bom`, `libs.quarkus.bom`,
  `libs.ktor.bom`). No version string in a module build file.
  - Do: `api(platform(libs.otel.bom)); api(libs.otel.api)`
  - Don't: `implementation("io.opentelemetry:opentelemetry-api:1.67.0")`
- Keep the OTel BOMs in lockstep: `otel` (1.67.0), `otel-bom-alpha` (`1.67.0-alpha`, hard-coded in the catalog) and
  `otelInstrumentation`/`otelInstrumentationAlpha` (2.33.0) are bumped together.
- Java compiles with `-Xlint:all,-serial,-processing,-options,-try -parameters`; javadoc runs `-Xdoclint:all,-missing`.
  Warnings are not fatal today, but don't add new ones.
- One version for all modules (`version` in [`gradle.properties`](../gradle.properties)); group `ai.tasktrooper`.

## Modules and language

| Module | Language | Role |
|---|---|---|
| `trooper-otel-core` | Java | DSN, `TrooperConfig`, sampler, span processor, propagator, profiling, `SdkErrors` |
| `trooper-otel-extension` | Java | javaagent extension (SPI); bundles core classes, never OTel |
| `trooper-javaagent` | (build only) | repackages upstream `opentelemetry-javaagent` + extension into `trooper-javaagent.jar` |
| `trooper-spring-boot-starter` | Java | Spring Boot 3 auto-config (no agent) |
| `trooper-quarkus` | Java | plain library for Quarkus 3 (no deployment module) |
| `trooper-llm` | Java | GenAI spans, Anthropic wrapper, LangChain4j listener |
| `trooper-kotlin`, `trooper-ktor` | Kotlin | `Trooper.init`, `withSpan`, coroutine propagation, Ktor 3 plugins |

- **Shared logic goes in `trooper-otel-core` (Java)**, so the agent, Spring, Quarkus and Kotlin paths behave the same.
  Kotlin modules only wrap core (see `Trooper.kt` → `TrooperSdk`, `TrooperConfig`).
- New Java code in Kotlin modules / Kotlin in Java modules: don't.
- `examples/*` are compiled by the build but never published (root `subprojects {}` skips `:examples`).

## Packages and naming

- Packages: `io.tasktrooper.telemetry.<module>` — `core`, `core.profiling`, `agent`, `javaagent`, `spring`, `quarkus`,
  `llm`, `kotlin`, `ktor`. (Maven coordinates are `ai.tasktrooper:*`; packages intentionally were not renamed.)
- Public types are prefixed `Trooper*` (`TrooperConfig`, `TrooperSampler`, `TrooperAutoConfiguration`); Ktor plugins
  use `TaskTrooper*`. Utility classes are `public final class` with a private constructor (`SdkErrors`, `OtlpRetry`).
- Kotlin follows `kotlin.code.style=official`; test names may use backticks (`` fun `withSpan keeps context`() ``).

## Dependencies — don't leak into the host

- Framework/integration deps are **`compileOnly`**; the host app brings them (Spring, Quarkus, Ktor server/client,
  LangChain4j, Kafka, Rabbit, spring-retry). Classes referencing optional deps must load only when used.
- `trooper-otel-extension` must contain only our classes: core is `compileOnly` and zipped into the jar; the agent
  provides OTel at runtime. Never add a runtime OTel dep there.
- `trooper-javaagent` = upstream agent jar + `extensions/trooper-otel-extension.jar`, no POM dependencies. Don't add
  shading/relocation; follow the upstream "distro" pattern in its `build.gradle.kts`.
- Guard optional classes: catch `NoClassDefFoundError` separately (see `OtlpRetry.span`).
- Auto-registration is SPI/metadata-file based: `META-INF/services/*` (extension, Quarkus),
  `META-INF/spring/...AutoConfiguration.imports` + `spring.factories` (Spring). Register new hooks the same way.

## Never crash the host

- Every SDK entry point (processor, sampler, propagator, interceptor, filter, listener, plugin) wraps its body in
  `try { … } catch (Throwable t) { SdkErrors.record("<component>", t); }` and falls back to a no-op / pass-through.
  Kotlin: `catch (t: Throwable)`.
- `SdkErrors.record` increments `trooper.sdk.errors{component}` (meter `TaskTrooper.Telemetry`, contracts §4) and
  logs at `FINE` only. It never throws.
- Component names are short snake_case, prefixed by integration: `span_processor`, `sampler`, `spring_route`,
  `quarkus_config`, `ktor_client`. Reuse an existing one when the place is the same.
- Wrappers around user calls (LLM, `withSpan`) record the error on the span and **rethrow the user's exception
  unchanged**; only SDK-internal failures are swallowed.

## Logging, nullability, config

- Logging is `java.util.logging` only (`Logger.getLogger(X.class.getName())`). No SLF4J/Log4j dependency; nothing
  above `FINE`/`WARNING` on hot paths; never `System.out`/`System.err`.
- No nullability annotation library. Public APIs document nulls in Javadoc; normalize at the edge (`blankToNull`,
  defaults in `TrooperConfig`). Kotlin options are nullable `var`s that fall back to `TrooperConfig.fromSystem()`.
- Config keys: `trooper.*` system properties / `TROOPER_*` env (`trooper.dsn`, `trooper.service`, `trooper.env`,
  `trooper.version`, `trooper.sample-rate`, `trooper.enabled`, `trooper.profiling.*`, `trooper.llm.capture-content`).
  Add a key as a constant in `TrooperConfig` + contracts.md first; translate to `otel.*` instead of re-implementing OTel.
- Network: export is OTel's async batch exporter; retry policy is `OtlpRetry.POLICY` (1s→60s, 5 attempts, contracts §2).
  Don't add blocking I/O on request threads.

## PII and cardinality

- Headers, bodies and query strings are not captured by default; LLM content only with `trooper.llm.capture-content`.
- `trooper.route` is always a template: use `RouteTemplates.normalize` / `RouteTemplates.grpc`, never a raw path.
- Don't put user/request ids into metric attributes.

## Docs and commits

- Javadoc on public types; reference contracts sections inline (`contracts.md §24`) like existing code does.
- New public API or config key → update [`.ai/sdk.md`](../../../.ai/sdk.md) / [`.ai/contracts.md`](../../../.ai/contracts.md)
  and [`README.md`](../README.md) in the same PR. English only; Conventional Commits (`feat(sdk-jvm): …`).
