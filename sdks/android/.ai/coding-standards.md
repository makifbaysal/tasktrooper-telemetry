# Android SDK — coding standards

Scope: `sdks/android` (Gradle KTS build: `trooper-android`, `trooper-ndk`, `trooper-okhttp`, `trooper-compose`,
`trooper-replay`, `trooper-gradle-plugin` (included build), `trooper-android-sample`).
Cross-cutting rules: [`../../../.ai/coding-standards.md`](../../../.ai/coding-standards.md).
Design and wire format: [`../../../.ai/sdk.md`](../../../.ai/sdk.md), [`../../../.ai/contracts.md`](../../../.ai/contracts.md)
(§2 DSN, §9 hybrid channel, §23 replay), RUM schema [`../../../proto/rum/v1/rum.proto`](../../../proto/rum/v1/rum.proto).
User-facing behaviour is documented in [`../README.md`](../README.md); keep it in sync with code changes.

## Toolchain (from the build files — change them deliberately)

| Item | Value | Where |
|---|---|---|
| AGP / Kotlin | 8.13.2 / 2.2.20 | `build.gradle.kts` |
| Gradle wrapper | 8.14 (vanniktech publish 0.35.x is the last line for Gradle 8) | `gradle/wrapper/gradle-wrapper.properties` |
| JDK toolchain | 21; bytecode Java/Kotlin 11 (plugin: 17) | each module `kotlin { jvmToolchain(21) … }` |
| `minSdk` / `compileSdk` | 21 / 36 for every library module | `*/build.gradle.kts` |
| NDK / CMake | 27.0.12077973 / 3.22.1, `-DANDROID_STL=none` | `trooper-ndk/build.gradle.kts` |
| Style | `kotlin.code.style=official`; no detekt/ktlint configured. Android Lint runs with defaults in `./gradlew build` | `gradle.properties` |

- One version for all modules + the plugin: `version` in `gradle.properties`. Don't set per-module versions.
- Code is Kotlin; the only C is `trooper-ndk/src/main/cpp` (async-signal-safe crash writer).

## Module layout

- Public API lives in the package root (`com.tasktrooper.android`: `TaskTrooper`, `TrooperConfig`, `RumModel`,
  `TrooperHybridBridge`). Everything else is `internal` under `…/internal/`.
- Optional modules talk to core only through `com.tasktrooper.android.spi` (`TrooperSpi`, `NativeCrashBridge`,
  `ReplayViewMapper`). SPI is "not a public API", but it must also be safe before `init` and never throw.
- Optional modules depend on core with `api(project(":trooper-android"))`, so apps add one artifact.
- Core finds optional modules **by reflection** (`Class.forName(NativeCrashBridge.IMPLEMENTATION)`); never add a
  compile dependency from core to `trooper-ndk` / `trooper-replay`. Keep their classes in `consumer-rules.pro`.

## Public API style

- Singleton `object TaskTrooper` + immutable `data class TrooperConfig` with defaults; only `dsn` and `app` required.
- Java-friendly: `@JvmStatic` on every entry point, `@JvmOverloads` where there are default args.
- Every public method: callable from any thread, no-op before `init`, never throws.
  ```kotlin
  @JvmStatic fun stopView(name: String? = null) { Internal.guard("stopView", Unit) { core?.stopView(name) } }
  ```
- `init` is idempotent (`synchronized` + `if (core != null) return`). Invalid DSN → SDK disabled, not an exception
  (`Dsn.parse` returns null).
- New config fields need a default that keeps existing behaviour, a KDoc line, and a row in README § Configuration.

## Never crash the host app

- Wrap SDK work in `Internal.guard(where, fallback) { … }` (`internal/Infra.kt`): it catches `Throwable`, increments
  `Internal.errorCount`, and logs only when `debug = true`.
- Background work goes through `Scheduler` / `ExecutorScheduler`, which wraps every task in `safely { }`.
- User callbacks (`beforeSend`) can throw — guard them.
- Do: return a fallback (`null`, `false`, `emptySet()`). Don't: `!!`, `error()`, `require()`, `check()` on paths
  reachable from the public API.
- Crash handler: write the crash synchronously, then **always chain the previous handler**
  (`CrashHandler` in `internal/AutoInstrumentation.kt`). Every step before chaining is in its own `try/catch`.
- Native handler (`trooper_crash.c`): async-signal-safe only (no malloc, locks, stdio), chain the previous
  signal handler. The host harness checks this — see [testing.md](testing.md).

## Threading

- Never do disk or network I/O on the main thread. Two single-thread daemon executors: `trooper-worker`
  (pipeline, disk) and `trooper-upload` (HTTP), both `MIN_PRIORITY`.
- Main thread work is limited to lifecycle callbacks and replay capture; replay capture has a CPU budget
  (1500 nodes / 12 ms, ≤ 2 fps). Don't add per-frame allocations or reflection on the main thread.
- Shared state: `@Volatile` fields or `synchronized`; `AtomicLong` for counters.
- Inject time and scheduling (`Clock`, `Scheduler`) instead of calling `System.currentTimeMillis()` / `Executors`
  directly, so logic stays testable with `FakeClock` / `FakeScheduler`.

## Delivery and persistence

- Batch ≤ 100 events or 5 s; flush on `ProcessLifecycleOwner` ON_STOP.
- Batches are gzipped to `noBackupFilesDir/tasktrooper/queue` (`DiskQueue`, 5 MB cap, oldest dropped,
  `Internal.droppedEvents`). Use `noBackupFilesDir` for any new SDK file — not `filesDir`, not external storage.
- HTTP: plain `HttpURLConnection` (`internal/Transport.kt`). 2xx delete; 429/5xx/IO → backoff 1 s → 60 s with
  jitter, `Retry-After` honoured; other 4xx drop.
- Requests to the ingest host are never instrumented (`TaskTrooper.isIngestUrl`).

## Privacy (PII off by default)

- Never collect headers, bodies or query values; URLs go through `UrlSanitizer`.
- User id / traits only via `identify`. `beforeSend` is the masking hook.
- Replay is wireframe only — never screenshots or pixels. Text masked by default; `EditText`, password fields and
  Compose text fields are **always** masked regardless of options. New view mappers must default to masked.

## Dependencies

- Core runtime deps: `androidx.lifecycle:lifecycle-process` only. Integrations the host may or may not have
  (fragment, activity, OkHttp, navigation-compose) are `compileOnly` and guarded at runtime.
- No JSON/HTTP/coroutine libraries in runtime: JSON is hand-written (`internal/JsonWriter.kt`).
- Pin exact versions; a new runtime dependency needs a reason in the PR.

## Commits

Conventional Commits with the scope used in history: `feat(sdk-android): …`, `fix(sdk-android): …`
(`sdk-mobile` when one change spans Android + iOS + Flutter). English everywhere.
