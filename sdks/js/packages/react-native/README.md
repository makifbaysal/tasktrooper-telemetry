# @trooper/react-native

```ts
// index.js — import first so app start (js_init) is measured from bundle start
import AsyncStorage from '@react-native-async-storage/async-storage'; // optional
import DeviceInfo from 'react-native-device-info';                   // optional
import { init, trooperNavigation } from '@trooper/react-native';

init({ dsn, app: 'courier-app', environment: 'production', storage: AsyncStorage, deviceInfo: DeviceInfo,
       tracePropagationTargets: ['https://api.example.com'] });

const navigationRef = createNavigationContainerRef();
const nav = trooperNavigation(navigationRef);
<NavigationContainer ref={navigationRef} onReady={nav.onReady} onStateChange={nav.onStateChange}>…
```

- Platform `PLATFORM_REACT_NATIVE`; os/device from `Platform` + `Dimensions`; model/manufacturer/app version from
  `react-native-device-info` if passed.
- Errors: `ErrorUtils.setGlobalHandler` (previous handler chained; fatal → `is_crash`, `source: crash`, immediate flush).
  Unhandled promise rejections via Hermes' rejection tracker — on by default in release builds only (it replaces RN's dev warning).
- Network: XHR instrumentation (RN's `fetch` is built on XHR, so both are covered once). `traceparent` only for targets.
- `AppState` background/inactive → VIEW_END + flush; active → the screen view resumes.
- Screens: react-navigation route name = view name; `VIEW_START.loading_time_ms` = time to the next frame.
  Without react-navigation: `setScreen(name)` + `markAppReady()`.
- APP_START (`cold`) on the first `onReady`: from native process start (`attributes.start_reference=process`) when the native
  bridge provides it, else from JS bundle start (`js_init`).
- Offline queue + anonymous id in AsyncStorage when `storage` is passed (≤ 5 MB, FIFO), memory otherwise. Sessions are in memory
  (a cold start after > 30 min idle is a new session anyway).

## Native SDKs / hybrid apps

Native crashes (Java/Kotlin/ObjC/Swift) are **out of scope** here — add the TaskTrooper Android / iOS SDKs. The hybrid bridge
(contracts.md §9, channel `dev.tasktrooper.telemetry/rum`) is used automatically, silently ignored if absent:
`getContext()` → JS adopts the native session/anonymous id (native started first), `setContext({session_id, anonymous_id,
view_id, view_name, user_id})` is pushed on every session/screen/user change so native crash reports share the context,
`getProcessStartTime()` → APP_START from process start. Override with `init({ nativeBridge })` / `setNativeBridge()`, or disable
with `nativeBridge: null`.

### Native module

This package ships the bridge as the native module **`TrooperRum`** (RN module names can't contain `/` or `.`), auto-linked by
the React Native CLI (Android `dev.tasktrooper.reactnative.TrooperRumPackage`, iOS pod `trooper-react-native`).
New Architecture (TurboModule, codegen spec `src/NativeTrooperRum.ts`, `TrooperRumSpec`) and the legacy bridge are both
supported. JS looks up `TurboModuleRegistry.get('TrooperRum')`, then `NativeModules.TrooperRum`, then
`NativeModules['dev.tasktrooper.telemetry/rum']` (a custom module of your own).

- **Process start is always available** (Android API 24+ via `Process.getStartElapsedRealtime()`, iOS via `sysctl`), so
  APP_START uses `start_reference=process` even without a native SDK.
- **Android:** if the app includes `trooper-android`, calls are forwarded to `com.tasktrooper.android.TrooperHybridBridge`
  (resolved by reflection — no compile dependency; a consumer ProGuard rule keeps it). Otherwise context calls are no-ops.
- **iOS:** forwarded to the `TaskTrooper` pod when this pod can import it (`#if canImport(TaskTrooper)`); run
  `TROOPER_RN_NATIVE_SDK=1 pod install` to make `trooper-react-native` depend on `TaskTrooper/Core`. Otherwise no-ops.

## Session replay

React Native draws native views, so the native SDKs' **wireframe** replay (ADR-021, contracts.md §23 — no screenshots or
pixels) records RN screens as they are. It is started from JS through the `TrooperRum` native module and needs the native SDK:

```ts
import { init, startReplay, stopReplay, isReplayRecording } from '@trooper/react-native';

init({ dsn, app: 'courier-app' });
const started = await startReplay({ sampleRate: 0.1, onErrorSampleRate: 1.0, maskAllText: true }); // all optional
```

- **Android:** add `trooper-android` (`TaskTrooper.init` in `Application.onCreate`) **and** `trooper-replay` to the app.
  The module finds `com.tasktrooper.android.replay.TrooperReplay` by reflection (no compile dependency; consumer ProGuard
  rules keep it) and calls `TrooperReplay.install(currentActivity, ReplayOptions(...))`.
- **iOS:** `TROOPER_RN_NATIVE_SDK=1 pod install` makes `trooper-react-native` depend on `TaskTrooper/Core` **and**
  `TaskTrooper/Replay` (and defines `TROOPER_RN_REPLAY`); start the native SDK with `TaskTrooper.start(config)` in the
  AppDelegate. `TROOPER_RN_REPLAY=0` keeps the native SDK without replay.
- Options are the native ones (missing keys = native defaults: `sampleRate` 0.1, `onErrorSampleRate` 1.0, `maskAllText`
  true; inputs are always masked). Out-of-range values are ignored. A replay already installed natively
  (`TrooperReplay.install` / `TaskTrooper.start(config, replay:)`) keeps its options — later installs are no-ops.
- **Session:** segments use the RUM session of the JS layer — it is pushed with `setContext` (see above), so the replay,
  the JS events and native crashes share one `session_id`. Call `startReplay()` after `init()`.
- **JS errors** (global handler, Hermes unhandled rejections, `captureError`) of a sampled session also call
  `notifyReplayError()` on the native module, so on-error sessions upload their last 60 s for JS errors too (the native
  replay only observes native errors by itself).
- **Without the native SDK / replay module** `startReplay()` is a no-op: it resolves `false` and logs one console warning.
  `isReplayRecording()` resolves `true` only while the session is recorded (not when it is just buffered for errors).
  `stopReplay()` resolves `true` when the native SDK stopped recording; the current native replay modules have no stop API
  yet, so it resolves `false` there. Nothing here ever throws or rejects.
