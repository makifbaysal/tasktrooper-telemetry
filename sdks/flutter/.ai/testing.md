# Flutter SDK — testing

Cross-cutting rules: [`../../../.ai/testing.md`](../../../.ai/testing.md). Code rules: [coding-standards.md](coding-standards.md).

## Gates — what actually runs

- **Not in PR CI.** `.github/workflows/ci.yml` and `make check` do not analyze or test `sdks/flutter`.
- **Release only:** `.github/workflows/release-flutter.yml` (tag `tasktrooper_flutter-vX.Y.Z` /
  `tasktrooper_dio-vX.Y.Z`) deletes `pubspec_overrides.yaml`, then runs `flutter pub get`, `flutter analyze`,
  `flutter test` for **that one package** against pub.dev dependencies, then `dart pub publish`.
  `example` and the native plugin code (Kotlin/Swift) are never built by a workflow.
- So: run the local commands below before every PR that touches `sdks/flutter`.

## Run locally

```bash
cd sdks/flutter/tasktrooper_flutter && flutter analyze && flutter test
cd sdks/flutter/tasktrooper_dio     && flutter analyze && flutter test
cd sdks/flutter/example             && flutter analyze       # + flutter run against a local ingest
flutter test test/pipeline_test.dart --name 'Retry-After'    # single file / test
dart format --set-exit-if-changed .                          # formatting (page_width 120)
```

Before a dio release, also run its tests **without** the override (`mv pubspec_overrides.yaml /tmp && flutter pub
get && flutter test`) — that is what the release gate does, and it fails if dio needs unreleased core API.

Native plugin changes: build `example` on a device/simulator (`flutter run`), on Android with and without
`trooper-android` on the classpath, on iOS with the `Core` and `Native` subspecs.

## Framework and layout

- `flutter_test` (`test(...)`, `testWidgets(...)`), `fake_async` for timers, `package:http/testing.dart`
  `MockClient` for HTTP, Dio's `HttpClientAdapter` for the interceptor.
- `tasktrooper_flutter/test/`: `pipeline_test` (batching, flush, retry/backoff, queue cap), `session_test`,
  `errors_test` (hook chaining), `http_client_test`, `trace_test`, `dsn_test`, `navigator_observer_test`,
  `native_bridge_test` (MethodChannel mock), `replay_mapper_test`, `replay_engine_test`, `replay_recorder_test`,
  `proto_schema_test`.
- `tasktrooper_dio/test/interceptor_test.dart`: `_FakeAdapter` records requests and simulates failures.
- `example` has no tests.

## Fakes and helpers (`tasktrooper_flutter/test/helpers.dart`)

- `testConfig(...)` — config with a fixed test DSN; override only what the test needs.
- `FakeIngest([statuses], headers)` — `MockClient` that records requests and answers with the given status list
  (then 200); exposes `batches` / `events` decoded from JSON.
- `MemoryStorage` (from `lib/src/storage.dart`) instead of disk; `TaskTrooper.createClient(...)` /
  `debugSetClient(...)` to wire a client without platform setup.
- Drive time with `fakeAsync((async) { …; async.elapse(...); })` — never real delays.

## Rules

- Payload changes: `proto_schema_test.dart` reads `../../../proto/rum/v1/rum.proto` (relative to the package dir,
  so run tests from `sdks/flutter/<package>`) and checks every serialized field/enum. Update the proto first.
- Error hooks: assert the previous `FlutterError.onError` / `PlatformDispatcher.onError` still run and are
  restored by the uninstaller.
- Transport: cover 2xx, 4xx drop, 408/429/5xx retry with `Retry-After`, offline queue cap, web (no gzip) when relevant.
- Replay mapper: assert `TextField` values and `obscureText` stay masked with `maskAllText: false`.
- Every public static method must be a no-op and not throw before `init`; there is no dedicated test for this yet
  (Android has `apiIsNoOpBeforeInit`) — add one when touching `task_trooper.dart`.
- Global state: reset in `tearDown` (`TaskTrooper.debugSetClient(null)`, `TrooperReplay.close`, mock channel
  handler `null`), as `http_client_test` / `replay_recorder_test` / `native_bridge_test` do.
- Naming: `<subject>_test.dart`; behaviour-style test names (`'429 honours Retry-After'`), `group(...)` optional.
