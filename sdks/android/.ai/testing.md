# Android SDK — testing

Cross-cutting rules: [`../../../.ai/testing.md`](../../../.ai/testing.md). Code rules: [coding-standards.md](coding-standards.md).

## Gates — what actually runs

- **Not in PR CI.** `.github/workflows/ci.yml` and `make check` do not build or test `sdks/android`.
- **Release only:** `.github/workflows/release-android.yml` (tag `android-vX.Y.Z`) installs NDK + CMake and runs
  `./gradlew build --no-configuration-cache` (all unit tests, Android Lint, minified sample, host native harness,
  Gradle plugin tests) before publishing. A failure there blocks the release, not the merge.
- So: **run the local commands below before every PR that touches `sdks/android`.**

## Run locally

```bash
cd sdks/android
./gradlew build                          # everything the release gate runs
./gradlew testDebugUnitTest              # JVM/Robolectric unit tests only (fast loop)
./gradlew :trooper-replay:testDebugUnitTest --tests '*ViewMapperTest'
./gradlew :trooper-ndk:hostNativeTest    # native crash harness on macOS/Linux (part of `check`)
```

Prereqs: JDK 21, `local.properties` with `sdk.dir=…` (git-ignored), and for `trooper-ndk`
`sdkmanager "ndk;27.0.12077973" "cmake;3.22.1"`.

## Frameworks and layout

| Module | Tests | Framework |
|---|---|---|
| `trooper-android` | `src/test/java/com/tasktrooper/android/internal/*Test.kt` (pipeline, uploader, session, crash, ANR, app start, exit info, schema) + `TaskTrooperRobolectricTest.kt` | JUnit 4, Robolectric 4.16 (`@Config(sdk = [35])`), Gson for asserting JSON |
| `trooper-replay` | `src/test/…/replay/internal/` (wireframe, mapper, engine, recorder, segment upload) | JUnit 4 + Robolectric |
| `trooper-okhttp` | `TrooperInterceptorTest.kt` | JUnit 4 + OkHttp `MockWebServer` |
| `trooper-compose` | `RouteNamesTest`, `ReplayKindTest` | plain JUnit 4 |
| `trooper-ndk` | `src/hostTest/cpp/crash_harness.c` | C harness: forks children that really crash (SIGSEGV, worker thread, SIGABRT, trap, chained previous handler) |
| `trooper-gradle-plugin` | `src/test/kotlin/…` | JUnit 4 + Gradle TestKit (incl. a real AGP minified app) + `FakeSymbolicator` HTTP server |

No instrumented (`androidTest`) tests and no emulator in any workflow.

## Rules

- Prefer pure JVM tests on `internal` classes; use Robolectric only when Android framework behaviour matters.
- Use the fakes in `trooper-android/src/test/…/internal/Fakes.kt`: `FakeClock` (advance time), `FakeScheduler`
  (`runDue()` runs due tasks deterministically), plus in-memory stores (`MemoryStore`). Don't `Thread.sleep`.
- Unit tests set `unitTests.isReturnDefaultValues = true`; don't rely on Android stubs returning real values.
- **Schema test is mandatory for payload changes:** `RumSchemaTest` parses `proto/rum/v1/rum.proto` at test time
  and checks every emitted key/enum. Changing an event → update the proto first (see `../../../.ai/sdk.md`), then the SDK.
- Every public entry point must have a "no-op before init / never throws" case
  (`TaskTrooperRobolectricTest.apiIsNoOpBeforeInit`). Reset global state in `@After`
  (`TaskTrooper.installForTest(null)`).
- Transport changes: cover 2xx / 4xx drop / 429 + `Retry-After` / 5xx backoff / offline queue cap (`UploaderTest`,
  `PipelineTest`).
- Crash changes: assert the previous `UncaughtExceptionHandler` is still called (`CrashTest`); native handler
  changes: extend `crash_harness.c`.
- Replay mapper changes: assert inputs/password fields stay masked even with `maskAllText = false`.
- Naming: `<Subject>Test.kt`, one class per subject, descriptive backtick-free method names (`apiIsNoOpBeforeInit`).

## Manual check

`trooper-android-sample` (Views + Compose screens) for on-device smoke tests. It runs the mapping-upload plugin
after `minifyReleaseWithR8` only when `TROOPER_API_URL` / `TROOPER_API_KEY` are set.
