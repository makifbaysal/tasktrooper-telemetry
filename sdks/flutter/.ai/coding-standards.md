# Flutter SDK — coding standards

Scope: `sdks/flutter` — `tasktrooper_flutter` (Dart SDK + Android/iOS plugin for the hybrid channel),
`tasktrooper_dio` (Dio interceptor), `example` (not published).
Cross-cutting rules: [`../../../.ai/coding-standards.md`](../../../.ai/coding-standards.md).
Design and wire format: [`../../../.ai/sdk.md`](../../../.ai/sdk.md), [`../../../.ai/contracts.md`](../../../.ai/contracts.md)
(§2 DSN, §9 `dev.tasktrooper.telemetry/rum` channel, §23 replay), RUM schema
[`../../../proto/rum/v1/rum.proto`](../../../proto/rum/v1/rum.proto). User docs: [`../README.md`](../README.md) — keep in sync.

## Toolchain

| Item | Value | Where |
|---|---|---|
| Dart / Flutter | `sdk: ^3.11.4`, `flutter: ">=3.27.0"` (both packages) | `pubspec.yaml` |
| Lints | `package:flutter_lints/flutter.yaml` + `always_declare_return_types`, `avoid_dynamic_calls`, `prefer_final_locals`, `unawaited_futures` | `analysis_options.yaml` (identical in both packages) |
| Formatter | `dart format`, `page_width: 120` | `analysis_options.yaml` |
| Plugin Android | Kotlin, `minSdk 21`, `compileSdk 36`, JVM 17 | `tasktrooper_flutter/android/build.gradle.kts` |
| Plugin iOS | Swift, `platform :ios, '13.0'`; subspecs `Core` (default, no native SDK) / `Native` (+ `TaskTrooper/Core`) | `tasktrooper_flutter/ios/tasktrooper_flutter.podspec` |

- `flutter analyze` must be clean (the release gate fails on it). Run `dart format .` before committing.
- `unawaited_futures` is on: fire-and-forget calls are wrapped in `unawaited(...)` explicitly.

## Package layout

- Public surface = `lib/tasktrooper_flutter.dart` exports only. Implementation in `lib/src/`; export new types
  deliberately and with `show` where only part of a file is public (`client.dart show TrooperClient, TrooperContext`).
- Platform split via conditional export (`lib/src/platform/platform.dart`: `_io` / `_web` / `_stub`). Don't import
  `dart:io` or `package:web` outside those files.
- Replay lives in `lib/src/replay/` (engine, mapper, wireframe, uploader, unmask, options).
- `tasktrooper_dio` depends on the **published** `tasktrooper_flutter: ^x.y.z`; `pubspec_overrides.yaml` points
  it (and `example`) at the local path for development and is excluded from the archive (`.pubignore`).
  A dio change that needs new core API → release `tasktrooper_flutter` first.

## Public API style

- `abstract final class TaskTrooper` with static methods; immutable `const TrooperConfig(...)` with defaults
  (`dsn`, `app` required); `TrooperReplayOptions` separate.
- `TaskTrooper.init(config, appRunner: () => runApp(...))` runs the app in `runZonedGuarded`; an invalid config
  disables the SDK but **still runs the app**. A second `init` is a no-op.
- Every static method: safe before/without `init` (`_client?.…`), never throws.
- Test seams are public but marked: `TaskTrooper.debugSetClient`, `TaskTrooper.createClient`, injectable
  `http.Client`, `TrooperStorage` (`MemoryStorage`), `clock` package.

## Never crash the host app

- Wrap SDK work in `try { … } catch (e, s) { _internalError(e, s); }` (`lib/src/client.dart`):
  increments `TrooperClient.internalErrors` (internal metric) and prints only with `config.debug`.
- Error hooks **chain**: `installErrorHandlers` (`lib/src/errors.dart`) reports, then calls the previous
  `FlutterError.onError` (or `FlutterError.presentError`) and the previous `PlatformDispatcher.onError`.
  Each report is in its own `try/catch`. Keep this pattern for any new global hook and return an uninstaller.
- MethodChannel calls tolerate `MissingPluginException` (web/desktop have no native side).
- Native plugin code (`TrooperFlutterPlugin.kt` / `.swift`) catches `Throwable` / errors and finds the native SDK
  by reflection (`com.tasktrooper.android.TrooperHybridBridge`) — no compile dependency on it.
- Don't use `!` on values that come from the platform, storage or user callbacks.

## Async and threading

- Everything runs on the main isolate; keep per-frame work tiny. Replay capture runs after a drawn frame,
  ≤ 2 fps, budget 1500 nodes / 8 ms (×4 in debug). Frame stats use `SchedulerBinding.addTimingsCallback`.
- Timers via `dart:async` `Timer`; time via `clock.now()` (package `clock`) rather than `DateTime.now()`, so
  `fake_async` tests work (`http_client.dart` and `platform_io.dart` still call `DateTime.now()` — don't copy that).
- Serialize disk writes through a single future chain (`RumPipeline._persisting`); don't add parallel writers.
- Clean up in `TaskTrooper.close()`: cancel timers, remove listeners/callbacks, restore hooks.

## Delivery and persistence

- Batch ≤ 100 events or 5 s; flush on `hidden`/`paused`. Gzip except on web. Header `x-trooper-key`.
- File queue `<app support dir>/tasktrooper/rum_queue.jsonl` (≤ 5 MB, oldest dropped); web: in memory /
  `localStorage` for the anonymous id.
- 429/5xx/408/network → backoff 1 s → 60 s with jitter, `Retry-After` honoured; other 4xx drop (`lib/src/transport.dart`).

## Privacy

- `TrooperHttpClient` / `TrooperDioInterceptor`: never record headers or bodies; strip query values.
- User data only via `identify`; `beforeSend` is the masking/drop hook.
- Replay: wireframes from the Element tree, never pixels. Text masked by default (length kept); text fields and
  `obscureText` **always** masked; images are boxes; platform views black boxes. Opt-in via `TrooperUnmask`.
  New widget mappings default to masked.
- Hybrid apps: Dart owns the session (`setContext`); never enable native replay alongside Dart replay.

## Dependencies

Runtime: `clock`, `device_info_plus`, `http`, `meta`, `path_provider`, `web` (+ `dio` in `tasktrooper_dio`).
Adding one requires a reason in the PR; prefer the SDK/standard library.

## Commits

`feat(sdk-flutter): …`, `fix(sdk-flutter): …` (`sdk-mobile` for cross-platform changes). English everywhere.
Bump `version` in `pubspec.yaml` + `CHANGELOG.md` per package; `lib/src/version.dart` must match.
