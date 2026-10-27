# TaskTrooper Telemetry — Flutter SDK

RUM for Flutter apps: sessions, screen views, errors, network requests, frame vitals, app start and wireframe session replay, sent as RUM JSON
(`proto/rum/v1/rum.proto`) to `POST /v1/rum` (ADR-011, `.ai/contracts.md` §2, §9).

| Package | What |
|---|---|
| [`trooper_flutter`](trooper_flutter) | Core SDK: `TaskTrooper`, `TrooperNavigatorObserver`, `TrooperHttpClient` (`package:http`) |
| [`trooper_dio`](trooper_dio) | `TrooperDioInterceptor` (separate so Dio is not forced on you) |
| [`example`](example) | go_router + http + Dio demo app |

## Install

```yaml
dependencies:
  trooper_flutter: ^0.1.0
  trooper_dio: ^0.1.0   # only if you use Dio
```

## Init

```dart
import 'package:trooper_flutter/trooper_flutter.dart';

void main() {
  TaskTrooper.init(
    const TrooperConfig(
      dsn: 'https://<ingest-key>@ingest.trooper.dev/<project_id>',
      app: 'courier-app',
      version: '1.4.2+87',
      environment: 'production',
      sampleRate: 1.0,
      tracePropagationTargets: ['api.shop.com'],
    ),
    appRunner: () => runApp(const MyApp()),
  );
}
```

`init` runs `appRunner` inside `runZonedGuarded` (and calls `WidgetsFlutterBinding.ensureInitialized()` inside the
same zone), and installs `FlutterError.onError` + `PlatformDispatcher.instance.onError` handlers that **chain** to
the previous handlers. An invalid DSN disables the SDK; the app still runs. No SDK call ever throws.

```dart
TaskTrooper.identify('user-123', traits: {'plan': 'pro'}); // opt-in; identify(null) clears
TaskTrooper.track('checkout_started', props: {'items': '3'}, value: 120);
TaskTrooper.captureError(error, stackTrace);
TaskTrooper.startView('Checkout'); TaskTrooper.stopView();   // manual views
TaskTrooper.addAction('add_to_cart', target: 'product_42');
await TaskTrooper.flush();
```

## Navigation

```dart
MaterialApp(navigatorObservers: [TrooperNavigatorObserver()], routes: {...});
```

- View name = `RouteSettings.name`. Use route **templates** (`/products/:id`), not concrete paths — the view name is
  a dashboard dimension (cardinality rule).
- Only `PageRoute`s with a name are views; dialogs, sheets and unnamed `MaterialPageRoute`s are ignored. Override with
  `routeFilter:` / `nameExtractor:`.
- VIEW_START is emitted after the screen's first frame and carries `loading_time_ms` (push → first frame); VIEW_END
  carries `time_spent_ms`, preceded by `SLOW_FRAMES` / `FROZEN_FRAMES` vitals for that view.

**go_router:** `GoRouter(observers: [TrooperNavigatorObserver()], ...)`. go_router sets the page name to
`GoRoute.name ?? GoRoute.path`, so give routes a `name:` (`GoRoute(name: 'ProductDetail', path: 'products/:id')`) or you
get the *relative* path template (`products/:id`). A `NavigatorObserver` can only watch one navigator: pass a new
instance to each `ShellRoute(observers: [...])` too.

## Network

```dart
final client = TrooperHttpClient();                 // package:http, wraps http.Client()
final dio = Dio()..interceptors.add(TrooperDioInterceptor());   // trooper_dio, add it first
```

Each request becomes a RESOURCE event (method, status — `0` on network error —, duration, sizes). For URLs matching
`tracePropagationTargets` a W3C `traceparent` (`00-<trace_id>-<span_id>-01`) is added and its ids are stored on the
event, linking the screen to the backend trace; `baggage: tt.svc=<app>` is added too (merged into an existing `baggage`)
so the backend knows the caller. Headers and bodies are never recorded; query values are stripped
(`?token=x&page=2` → `?token&page`). Use `beforeSend` for further masking.

## What is collected

| Event | Source |
|---|---|
| SESSION_START | new session: 30 min inactivity or 4 h max; `sampleRate` decided per session |
| VIEW_START / VIEW_END | navigator observer or `startView`/`stopView`; view ends on background, restarts on resume |
| VITAL `SLOW_FRAMES`, `FROZEN_FRAMES` | `SchedulerBinding.addTimingsCallback`; slow = build or raster > 16 ms, frozen = total > 700 ms; counts per view, `total_frames` attribute, rating by slow ratio (≤5 % good, ≤10 % needs-improvement) |
| APP_START (`cold`) | init → first rasterized frame (`WidgetsBinding.waitUntilFirstFrameRasterized`), see below |
| ERROR | Flutter framework, platform dispatcher, zone and manual errors; `is_crash: false` |
| RESOURCE | `TrooperHttpClient`, `TrooperDioInterceptor` |
| ACTION / CUSTOM | `addAction`, `track` |

**App start.** Dart cannot read the process start time. If the native bridge (below) answers `getProcessStartTime`,
the duration is process start → first frame (`start_reference: process`). Otherwise it is `TaskTrooper.init` (≈ Dart
`main`) → first frame (`start_reference: dart_init`), which misses engine/VM startup (typically 100–400 ms on Android).

**Device / OS** come from `device_info_plus` (model, manufacturer, OS version), screen size and locale from
`PlatformDispatcher`. `connection` is reported as `unknown`.

## Session replay

```dart
TaskTrooper.init(
  const TrooperConfig(dsn: '...', app: 'courier-app'),
  appRunner: () => runApp(const MyApp()),
  replay: const TrooperReplayOptions(sampleRate: 0.1, onErrorSampleRate: 1.0),
);
```

Wireframe replay (ADR-021, `.ai/contracts.md` §23), recorded in Dart from the **Element tree** — Flutter paints into a
single `FlutterView`, so the native SDKs' view traversal cannot see it. **No screenshots or pixels are captured.**

- **What a frame is:** a flat, back-to-front list of nodes (`kind`, logical-pixel rect, colors, radius, masked text).
  Widget type → kind: `Text`/`RichText` → `text`, `TextField`/`CupertinoTextField`/`EditableText` → `input`, `Image` →
  `image` (box only), `Icon` → `icon`, Material buttons / `IconButton` / FAB / `CupertinoButton` → `button`, `Switch` /
  `Checkbox` / `Radio` (+ Cupertino) → `switch`, scroll views → `list`, platform views → `webview` (black box).
  Backgrounds come from `Material`/`Card`/`AppBar`/`Scaffold` (physical shape), `Container`/`DecoratedBox`
  (`BoxDecoration`/`ShapeDecoration`: color, gradient's first color, radius, border) and `ColoredBox`. Bounds come from
  each `RenderBox` (paint transforms) clipped by scroll viewports / `ClipRect`…; `Offstage`, `Visibility`, opacity 0,
  routes covered by an opaque route and fully clipped subtrees are skipped. Dialogs, sheets and menus (the `Overlay`) are
  included.
- **When:** only when a frame was drawn since the last capture (persistent frame callback → dirty flag), at most 2 fps;
  the interval grows (≈ 1 % of main-isolate time, up to 5 s) when a capture is expensive. A capture stops at 1500 nodes or
  8 ms (×4 in debug builds). Identical frames are not recorded twice. Touches (`down`/`move` ≤ 10/s/`up`) via the global
  pointer route; keyboard visibility from the view insets.
- **Privacy:** all text is masked by default, keeping its length (`Pay now` → `••• •••`); text field values and
  `obscureText` are **always** masked; images are boxes. Show text of a subtree with `TrooperUnmask(child: …)`
  (`TrooperUnmask(unmask: false, …)` masks a part again); `maskAllText: false` shows all non-input text.
- **Sampling:** decided once per RUM session (and only for RUM-sampled sessions): `sampleRate` (default 0.1) records;
  otherwise `onErrorSampleRate` (default 1.0) keeps the last 60 s in memory and, on the first ERROR (Flutter/Dart error
  hooks, `captureError`), uploads it and keeps recording. The decision and the segment counter are persisted
  (`state.json`, key `replay_state`), so a restarted app continues the same session's numbering.
- **Delivery:** segments of ≤ 5 s or ≤ 256 KB (each starts with a `screen` event) are gzipped and POSTed to
  `/v1/replay` with the ingest key; 429/5xx/network errors back off 1 s → 60 s, other 4xx drop. Unsent segments wait in
  `<app support dir>/tasktrooper/replay/` (≤ 2 MB, `maxQueueBytes`; new segments are dropped when full). Going to
  background seals and uploads the current segment. `resource.platform` is `android`/`ios`, `sdk_name` `trooper_flutter`.
- **Platforms:** Android and iOS. Not started on web (use `@trooper/replay`) or desktop.
- **Inspect:** `TrooperReplay.isRecording`, `TrooperReplay.stats` (captures, avg/max cost per capture, nodes),
  `TrooperReplay.flush()`. With `debug: true` the stats are printed every 20 captures.

**Hybrid apps:** the session belongs to the Dart SDK (shared with the native SDK via `setContext`). **Do not enable the
native replay** (`TrooperReplay.install` from `trooper-replay` on Android, `TaskTrooper/Replay` on iOS) in a Flutter app:
it would record the `FlutterView` as one box and write the same session's segment numbers. On Android the plugin detects
an installed native replay (`isNativeReplayActive` on the channel) and the Dart recorder is then not started (warning in
the log); on iOS this cannot be detected, so the rule is documentation-only.

| Replay option | Default | |
|---|---|---|
| `sampleRate` | `0.1` | share of sessions recorded from the start |
| `onErrorSampleRate` | `1.0` | share of the other sessions buffered and uploaded on error |
| `maskAllText` | `true` | inputs are always masked |
| `enabled` | `true` | |
| `maxQueueBytes` | 2 MB | offline segment store |
| `minCaptureInterval` / `maxNodes` / `captureBudget` | 500 ms / 1500 / 8 ms | |

## Delivery

Events are batched (≤ 100 events or 5 s), gzip-compressed (not on web), POSTed with `x-trooper-key`. Batches are kept
in a file queue (`<app support dir>/tasktrooper/rum_queue.jsonl`, ≤ 5 MB, oldest dropped first) until accepted; on web
the queue is in memory. 429/5xx/408/network errors retry with exponential backoff 1 s → 60 s with jitter (`Retry-After`
honoured); other 4xx drop the batch. The app going to background (`hidden`/`paused`) flushes immediately. The anonymous
id is a UUID persisted in the same directory (web: `localStorage`).

## Configuration

| Option | Default | |
|---|---|---|
| `dsn` | required | `https://<key>@<host>[:port]/<project_id>` |
| `app` | required | client app service name |
| `version` | `''` | app version / build (deploy marker) |
| `environment` | `production` | |
| `sampleRate` | `1.0` | share of sessions recorded |
| `tracePropagationTargets` | `[]` | `String` (substring) or `RegExp` patterns for `traceparent` |
| `beforeSend` | null | mutate or drop (`null`) each event — PII masking |
| `attributes` | `{}` | extra resource attributes |
| `captureErrors` / `trackFrames` / `trackAppStart` / `trackLifecycle` | `true` | auto-instrumentation switches |
| `nativeBridge` | `true` | share context with native SDKs over a MethodChannel |
| `flushInterval` / `maxBatchSize` / `maxQueueBytes` | 5 s / 100 / 5 MB | |
| `sessionInactivityTimeout` / `sessionMaxDuration` | 30 min / 4 h | |
| `debug` | `false` | print internal SDK errors |

## Native crashes

Dart cannot observe native crashes (SIGSEGV, uncaught Kotlin/Swift exceptions, ANR, OOM). Install the native SDKs
(`sdks/android`, `sdks/ios`) side by side; they report those with `is_crash: true` and their own platform. To put them
in the same session, the Flutter SDK talks to them over `MethodChannel('dev.tasktrooper.telemetry/rum')`:

| Direction | Method | Payload |
|---|---|---|
| Dart → native | `setContext` | `{session_id, anonymous_id, view_id, view_name, user_id, sampled}` — sent on every change; `sampled` is the Dart RUM sampling decision (the native SDK adopts the session as external and never rotates it) |
| Dart → native | `getProcessStartTime` | returns process start, epoch ms (`int`) — improves APP_START |
| Dart → native | `isNativeReplayActive` | `bool` — the native replay is installed (Android; iOS always `false`), see [Session replay](#session-replay) |
| native → Dart | `getContext` | returns the same map |

`trooper_flutter` is a Flutter plugin (Android + iOS) that implements the native side of this channel. Web and
desktop have no implementation; there the calls fail silently (`MissingPluginException` is swallowed).

### Native plugin / hybrid apps

- **Process start** (`getProcessStartTime`) always works, even without a native SDK: Android API 24+
  (`Process.getStartElapsedRealtime()`; below API 24 → `null`), iOS via `sysctl(KERN_PROC_PID)` `p_starttime`. So
  APP_START uses `start_reference: process` on Android/iOS and `dart_init` elsewhere.
- **Android:** add `trooper-android` (`com.tasktrooper.telemetry`) to your app and start it. The plugin finds
  `com.tasktrooper.android.TrooperHybridBridge` by reflection (no compile dependency) and forwards `setContext` /
  `getContext` / `getProcessStartTime` to it. Without it `setContext` is a no-op and `getContext` returns `null`.
  The plugin ships an R8 keep rule for that class.
- **iOS (CocoaPods):** add the `TaskTrooper` pod and the plugin's `Native` subspec to `ios/Podfile` inside
  `target 'Runner'`; the plugin then calls `TaskTrooper.setHybridContext` / `hybridContext()` / `processStartTimeMs()`:

  ```ruby
  pod 'TaskTrooper', :git => 'https://github.com/tasktrooper/telemetry.git', :tag => 'ios-v0.1.0'
  pod 'trooper_flutter/Native', :path => '.symlinks/plugins/trooper_flutter/ios'
  ```

- **iOS (TaskTrooper via SPM in the app target):** implement `TrooperFlutterHybridBridge` in `AppDelegate` (three
  methods forwarding to `TaskTrooper`) and assign it to `TrooperFlutterPlugin.hybridBridge` before `runApp`.

With a native SDK present, native crashes / ANRs / OOMs are reported in the Dart session (the native SDK adopts the
Dart session id and does not emit its own SESSION_START).

## Development

```bash
cd trooper_flutter && flutter analyze && flutter test
cd trooper_dio && flutter analyze && flutter test
cd example && flutter analyze && flutter run --dart-define=TROOPER_DSN=http://tt_dev_ingest_key@10.0.2.2:4318/1
```
