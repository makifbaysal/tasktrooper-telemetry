# TaskTrooper Android SDK (RUM)

Kotlin RUM SDK for Android. Sends `RumBatch` JSON (schema: [`proto/rum/v1/rum.proto`](../../proto/rum/v1/rum.proto))
to `POST /v1/rum` on ingest-gw (gzip, `x-trooper-key`), per ADR-011 and `.ai/contracts.md` §2/§9.

| Module | What |
|---|---|
| `trooper-android` | Core: init, sessions, views, app start, crashes, ANR, frames, batching, offline queue. No OkHttp dependency. |
| `trooper-ndk` | Optional native (NDK) crash handler (C, async-signal-safe). Depends on `trooper-android`; picked up automatically. |
| `trooper-gradle-plugin` | Gradle plugin `com.tasktrooper.mapping-upload` (included build): uploads R8 `mapping.txt`. |
| `trooper-okhttp` | OkHttp `Interceptor` → `RESOURCE` events + W3C `traceparent` injection. |
| `trooper-compose` | Navigation Compose screen tracking + `Modifier.trooperAction()`. |
| `trooper-android-sample` | Sample app wiring all three. |

minSdk 21, compileSdk 36. Built with AGP 8.13, Kotlin 2.2, Gradle 8.14 (wrapper).

## Setup

```kotlin
// build.gradle.kts
dependencies {
    implementation("com.tasktrooper:trooper-android:0.1.0")
    implementation("com.tasktrooper:trooper-ndk:0.1.0")      // optional: native crashes
    implementation("com.tasktrooper:trooper-okhttp:0.1.0")   // optional
    implementation("com.tasktrooper:trooper-compose:0.1.0")  // optional
}
```

```kotlin
class App : Application() {
    override fun onCreate() {
        super.onCreate()
        TaskTrooper.init(this, TrooperConfig(
            dsn = "https://<ingest-key>@ingest.example.com/<project_id>",
            app = "courier-app",
            version = BuildConfig.VERSION_NAME,
            environment = "production",
            sampleRate = 1.0,
            tracePropagationTargets = listOf(Regex("^https://api\\.example\\.com")),
        ))
    }
}
```

Local stack: `http://tt_dev_ingest_key@10.0.2.2:4318/1` from the emulator.

```kotlin
TaskTrooper.identify("user-42", mapOf("plan" to "pro"))   // opt-in traits; never inferred
TaskTrooper.track("checkout_started", mapOf("items" to "3"), value = 120.0)
TaskTrooper.captureError(e, mapOf("screen" to "cart"))
TaskTrooper.startView("CustomScreen"); TaskTrooper.stopView()
TaskTrooper.addAction("pay_button")
TaskTrooper.reportFullyDrawn()        // TTFD for screens that are not androidx ComponentActivity
TaskTrooper.notifyActivity()          // dead-tap detection: UI changed in a way the SDK cannot see
TaskTrooper.flush()
```

All methods are no-ops before `init`, thread-safe, and never throw. Invalid DSN → SDK stays disabled.

### OkHttp

```kotlin
val client = OkHttpClient.Builder().addInterceptor(TrooperInterceptor()).build()
```

Every request becomes a `RESOURCE` event (method, status, duration, sizes, URL with query **values** stripped).
For URLs matching `tracePropagationTargets`, a `traceparent: 00-<trace_id>-<span_id>-01` header is added and the ids
are recorded on the event so the screen joins the backend trace. An existing `traceparent` is kept. The same requests get
`baggage: tt.svc=<app>` (merged into an existing `baggage` header) so the backend knows the caller. Requests to the
ingest host are ignored. Headers and bodies are never captured.

### Jetpack Compose

```kotlin
val nav = rememberNavController()
TrooperNavigationTracking(nav)          // route template → view name: "product/{id}" → "product/:id"
Button(onClick = ::pay, modifier = Modifier.trooperAction("pay")) { Text("Pay") }
```

Without Compose: `navController.addOnDestinationChangedListener(TrooperDestinationListener())`.
`navigation-compose` is `compileOnly`; the app provides it.

## What is collected automatically

| Signal | Event | Notes |
|---|---|---|
| Sessions | `SESSION_START` | Rotated after 30 min inactivity or 4 h. Anonymous id persisted in SharedPreferences. Sampling per session. |
| Activity views | `VIEW_START` / `VIEW_END` | Name = activity simple name. VIEW_START is deferred to the first frame and carries `loading_time_ms` (onCreate → first frame; its `ts_ms` is still the view start). VIEW_END carries `time_spent_ms`. |
| Fragment views | same | Via `FragmentLifecycleCallbacks` when androidx.fragment is on the classpath (NavHostFragment/dialogs ignored). |
| App start | `APP_START` | cold (process start via `Process.getStartUptimeMillis()` → first frame, `attributes.start_reference=process`; API < 24 measures from SDK init and omits it), warm (activity re-created), hot (resume). |
| Crashes | `ERROR` `source=crash`, `is_crash=true` | Written to disk synchronously in the uncaught handler (previous handler chained); uploaded on next launch. |
| Native crashes | `ERROR` `source=crash`, `is_crash=true`, `type` = `SIGSEGV`… | With `trooper-ndk`; see [Native crashes](#native-crashes-trooper-ndk). Attributes `native=true`, `abi`. |
| Previous-process exits | `ERROR`, `attributes.exit_info=true` | API 30+ `ApplicationExitInfo`; see [Exit reasons](#exit-reasons-api-30). |
| ANR | `ERROR` `source=anr` | Main-thread watchdog (5 s) with main thread stack; once per freeze; paused while a debugger is attached. |
| Frames | `VITAL` `SLOW_FRAMES` / `FROZEN_FRAMES` | Counts per view + `attributes.total_frames`; slow > 16 ms, frozen > 700 ms. API 24+: `Window.OnFrameMetricsAvailableListener` (real frame duration). API 21–23: Choreographer fallback — a frame callback re-posted while an activity is resumed; the vsync-to-vsync interval is the frame duration (≤ 17.5 ms counts as on time, contracts.md §9 jitter tolerance). The fallback also counts idle vsyncs, so `total_frames` is higher and the slow ratio lower than with FrameMetrics. |
| TTFD | `VITAL` `TTFD` (ms) | Activity `onCreate` → androidx `ComponentActivity.reportFullyDrawn()` (hooked via `FullyDrawnReporter`, also covers `ReportDrawn*` composables), or view start → `TaskTrooper.reportFullyDrawn()`. Once per view; rating good ≤ 2 s, poor > 4 s. Nothing is sent if the app never reports. |
| Dead taps | `ACTION.dead` | `true` when no navigation (view start), network request (OkHttp interceptor start / `recordResource`) or layout pass (`OnGlobalLayoutListener` on the decor view) happens within 1 s of the tap. ACTION events are therefore sent 1 s after the tap (`ts_ms` = tap time). Compose-only changes without a View layout pass are invisible: call `TaskTrooper.notifyActivity()` or set `trackDeadTaps = false`. |
| View url/referrer | — | Always empty on mobile (rum.proto: `url` is web-only); the screen is identified by `view_name`. |
| Connection | `connection` on every event | wifi / cellular / offline / unknown. |
| Device/OS | batch `resource` | manufacturer, model, type (mobile/tablet/tv), locale, screen, OS version, API level. |

## Delivery

- Batch ≤ 100 events or 5 s, whichever first; flush when the app goes to background (`ProcessLifecycleOwner` ON_STOP).
- Batches are written (gzipped) to `noBackupFilesDir/tasktrooper/queue` before upload; the queue is capped at 5 MB (oldest dropped first) and drained on next launch.
- 2xx → delete; 429/5xx/network error → exponential backoff 1 s → 60 s with jitter (honours `Retry-After`); other 4xx → drop.
- Upload uses `HttpURLConnection` on a background thread. Requires `INTERNET` + `ACCESS_NETWORK_STATE` (merged from the library manifest).

## Configuration

| Field | Default | Description |
|---|---|---|
| `dsn` | — | `https://<key>@<host>[:port]/<project_id>` |
| `app` | — | Client app (service) name |
| `version` | versionName+versionCode | Deploy marker |
| `environment` | `production` | |
| `sampleRate` | `1.0` | Fraction of sessions kept |
| `tracePropagationTargets` | `[]` | Regexes; matching URLs get `traceparent` |
| `trackActivities` / `trackFragments` | `true` | Automatic views |
| `trackAppStart` / `trackCrashes` / `trackAnr` / `trackFrames` | `true` | Automatic signals |
| `trackDeadTaps` | `true` | 1 s dead-tap window on ACTION |
| `trackNativeCrashes` | `true` | Only with `trooper-ndk` on the classpath |
| `trackExitReasons` | `true` | `ApplicationExitInfo` (API 30+) |
| `anrThresholdMs` | `5000` | ANR watchdog threshold |
| `maxBatchSize` | `100` | Max events per batch (capped at 100) |
| `flushIntervalMs` | `5000` | Max time an event waits in memory |
| `maxQueueBytes` | `5 MB` | Offline queue cap |
| `resourceAttributes` | `{}` | Extra `resource.attributes` |
| `beforeSend` | `null` | `(RumEvent) -> RumEvent?`: mask or drop (null) events |
| `debug` | `false` | Logcat output (tag `TaskTrooper`) |

## Hybrid apps (Flutter / React Native)

`TrooperHybridBridge` implements the shared context of contracts.md §9 (channel `dev.tasktrooper.telemetry/rum`):
`setContext(map)` (session_id, anonymous_id, view_id, view_name, user_id), `getContext()`, `getProcessStartTime()` (unix ms).
Flutter: the `trooper_flutter` plugin's MethodChannel `dev.tasktrooper.telemetry/rum`; React Native: the native module
`TrooperRum` (`TrooperHybridBridge.RN_MODULE`; JS falls back to a module named `dev.tasktrooper.telemetry/rum`). Those
wrappers belong to the Flutter/RN SDKs and forward to these methods (depend on `com.tasktrooper:trooper-android`); native
crashes are then reported with the Dart/JS session and view. In hybrid apps consider `trackActivities = false` so the
host activity does not appear as a screen.

## Privacy

No headers, bodies or query values are collected. User id and traits only via `identify`. Use `beforeSend` to mask
anything else (e.g. view names, error messages).

## Native crashes (`trooper-ndk`)

Adding the artifact is enough: `TaskTrooper.init` loads it reflectively (SPI `com.tasktrooper.android.spi.NativeCrashBridge`)
and installs handlers for SIGSEGV, SIGABRT, SIGBUS, SIGILL, SIGFPE and SIGTRAP (`SA_ONSTACK`; an alternate stack is
created for the installing thread, bionic gives every other pthread its own). Previous handlers are saved and chained, so
debuggerd still writes its tombstone and other crash reporters keep working; ART's own handlers (implicit null checks,
stack overflow) run first through libsigchain, so Java exceptions are not reported twice.

In the handler only async-signal-safe calls are used: a report file pre-opened at init
(`noBackupFilesDir/tasktrooper/ndk/<pid>-<start ms>.ndk`), static buffers, `write`, `prctl(PR_GET_NAME)`, `gettid`,
`clock_gettime`. The stack comes from `_Unwind_Backtrace` (warmed up at install; it is not formally async-signal-safe,
the usual trade-off) starting at the faulting pc of the signal context; if the unwinder cannot get through the signal
frame, a frame-pointer walk from the context registers is used, with every read validated through a pipe so a corrupt
stack cannot fault again. Frames are mapped to modules using a table captured outside the handler with
`dl_iterate_phdr` (path, address range, load bias, GNU build-id from `PT_NOTE`); it is refreshed from a background
thread when the screen changes (at most every 10 s), so libraries loaded later are covered. Session/view/user ids are
pushed into a double-buffered static string on every change and copied into the report.

On the next launch the report becomes an ERROR attributed to the crashed session and view (`ts_ms` = crash time),
`type` = signal name, message `Fatal signal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0 in tid … (thread)`, and
the stack in Android tombstone format, which `ndk-stack` and `services/symbolicator` parse (match `.so` files by build-id):

```
signal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0000000000000000
abi: 'arm64-v8a'
backtrace:
      #00 pc 000000000001a2b4  /data/app/~~x/com.example-1/lib/arm64/libapp.so (BuildId: 5f2d8e01aa77b0c3…)
      #01 pc 000000000001a2fc  /data/app/~~x/com.example-1/lib/arm64/libapp.so (BuildId: 5f2d8e01aa77b0c3…)
      #02 pc 0000000000001230  <unknown>
```

`pc` is module-relative (ELF vaddr = absolute pc − load bias). Frames ≥ 1 are return addresses minus the
libunwindstack adjustment (4 arm64, 2 arm, 1 x86/x86_64) so they point into the call instruction. Frames outside any
known module show the absolute pc and `<unknown>`. Keep unstripped `.so` files (or upload them, same build-id) to
symbolicate. Limitations: only the crashing thread is captured; no register dump or abort message; JNI code compiled
without unwind tables may stop early (the fp fallback helps only with `-fno-omit-frame-pointer`).

**Testing:** the handler + unwinder are exercised on the build machine: `./gradlew :trooper-ndk:hostNativeTest`
(part of `check`) compiles `trooper_crash.c` for macOS/Linux with CMake and runs `src/hostTest/cpp/crash_harness.c`,
which forks children that really crash (SIGSEGV in a nested call, on a named worker thread, SIGABRT, trap, and with a
previous handler that must still run) and checks the report (signal, pid, thread, context, first frame inside the
faulting function, caller frame, module with build-id). The Kotlin conversion is covered by JVM tests
(`NativeCrashTest`). No emulator system image is installed in CI, so there is no on-device test yet.

## Exit reasons (API 30+)

On launch, `ActivityManager.getHistoricalProcessExitReasons` is read and exits newer than the last processed one are
reported (the first run only stores a watermark; no history is replayed). Each becomes an ERROR attributed to the
session/view that process last had (`noBackupFilesDir/tasktrooper/proc/<pid>.ctx`), `is_crash=true`, attributes
`exit_info=true`, `exit_reason`, `exit_status`, `exit_importance`:

| Reason | `type` / `source` | Skipped when |
|---|---|---|
| ANR | `ANR` / `anr`, stack = `"main"` thread of the system ANR trace | the watchdog already reported an ANR (or a crash) for that pid |
| Native crash | signal name (`status`) / `crash` | a `trooper-ndk` report for that pid was sent, or a Java crash was recorded |
| Java crash | `JavaCrash` / `crash` | the uncaught handler already reported it (e.g. crash before `init` is still reported) |
| Low memory kill / excessive resource use | `LowMemoryKill` / `ExcessiveResourceUsage`, `crash` | the app was not visible (importance > 200) — background kills are normal |
| Initialization failure | `InitializationFailure` / `crash` | — |

User-requested stops, `exit()` and other reasons are ignored.

## ProGuard / R8

The library ships consumer rules; nothing to add. Crash stacks from minified builds are obfuscated; upload
`mapping.txt` for every release with the Gradle plugin:

```kotlin
// settings.gradle.kts: the plugin is not published yet; use it from this repo as an included build
pluginManagement { includeBuild("path/to/sdks/android/trooper-gradle-plugin") }

// app/build.gradle.kts
plugins { id("com.tasktrooper.mapping-upload") }
trooper {
    apiUrl.set("https://symbolicator.example.com")   // or env TROOPER_API_URL
    apiKey.set(providers.environmentVariable("TROOPER_API_KEY"))
    app.set("courier-app")                            // = TrooperConfig.app
}
```

After `minify<Variant>WithR8`, `uploadTrooperMapping<Variant>` posts `mapping.txt` to `POST <apiUrl>/v1/artifacts`
(multipart: `kind=r8`, `app`, `release`, file part `file`; `Authorization: Bearer`), same contract as
`trooper-cli upload-mapping`. `release` = `versionName+versionCode`, the SDK's default RUM `resource.version`; set
`trooper { release.set(...) }` if you pass a custom `TrooperConfig.version`. Missing credentials or upload errors only
warn (`failOnError.set(true)` to fail the build). The plugin is configuration-cache compatible; tests (fake HTTP server +
Gradle TestKit, including a real AGP 8.13 minified app) run with `./gradlew build`.

## Development

```bash
./gradlew build test            # all modules, unit tests, lint, minified sample
./gradlew testDebugUnitTest     # tests only
./gradlew publishToMavenLocal   # com.tasktrooper:{trooper-android,trooper-ndk,trooper-okhttp,trooper-compose}:0.1.0
                                # + com.tasktrooper:trooper-gradle-plugin and the com.tasktrooper.mapping-upload marker
```

`local.properties` (git-ignored) must contain `sdk.dir=...`. `trooper-ndk` needs NDK 27.0.12077973 and CMake 3.22.1
(`sdkmanager "ndk;27.0.12077973" "cmake;3.22.1"`); the host harness uses the SDK's CMake/Ninja (or `cmake` on PATH). Tests are JVM/Robolectric: session rotation,
batching/flush, retry/backoff, disk queue cap, crash persistence + resend, ANR watchdog (fake looper/clock), app start
classification, payload JSON vs. `rum.proto` (parsed at test time), OkHttp interceptor (MockWebServer), Compose route naming.

## Not yet supported

Protobuf transport; native crash stacks of threads other than the crashing one; publishing the Gradle plugin to the
Plugin Portal; on-device (instrumented) native crash tests.
