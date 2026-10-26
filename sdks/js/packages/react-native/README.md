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
