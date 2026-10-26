# TaskTrooper iOS SDK (RUM)

Real User Monitoring for iOS / tvOS apps and app extensions (SPM: iOS 13+, CocoaPods: iOS 15+ — see
[Deployment target](#deployment-target)). Sends RUM JSON batches (`proto/rum/v1/rum.proto`) to
`POST /v1/rum` (gzip, `x-trooper-key`). Only system frameworks are used.

Collected automatically: sessions (30 min inactivity / 4 h max), UIKit screens, app start (cold / warm / hot),
URLSession requests with W3C `traceparent`, crashes (signals + uncaught `NSException`, all threads, Swift
`fatalError` messages), main-thread hangs with the main thread's stack, slow/frozen frames per view. No headers, bodies or query values are collected.

## Install

**Swift Package Manager** — add the repository and pick the `TaskTrooper` product (plus `TaskTrooperSwiftUI` for SwiftUI):

```swift
.package(url: "https://github.com/tasktrooper/telemetry.git", from: "0.1.0") // package lives in sdks/ios
```

**CocoaPods**

```ruby
pod 'TaskTrooper'            # core
pod 'TaskTrooper/SwiftUI'    # + .trooperView modifier
```

With CocoaPods everything is one module: `import TaskTrooper` only.

## Setup

### UIKit

```swift
import TaskTrooper

func application(_ app: UIApplication, didFinishLaunchingWithOptions opts: [UIApplication.LaunchOptionsKey: Any]?) -> Bool {
    TaskTrooper.start(TrooperConfig(
        dsn: "https://<ingest-key>@ingest.example.com/<project-id>",
        app: "shop-ios", version: TrooperConfig.bundleVersion,   // "1.4.0 (812)" — also the dSYM release
        environment: "production",
        tracePropagationTargets: ["api.shop.com"]))
    return true
}
```

Screens are tracked from `viewDidAppear` / `viewDidDisappear` (swizzled; turn off with `trackViews = false`).
Containers (`UINavigationController`, `UITabBarController`, `UIPageViewController`, `UISplitViewController`),
`_UI*` and other system controllers and `UIHostingController`s are ignored. The name is the class name; override it
(or return `nil` to skip a controller) with:

```swift
extension ProductViewController: TrooperViewNaming {
    var trooperViewName: String? { "ProductDetail" }
}
```

### SwiftUI

```swift
import TaskTrooper
import TaskTrooperSwiftUI

@main struct ShopApp: App {
    init() { TaskTrooper.start(TrooperConfig(dsn: "...", app: "shop-ios", version: "1.4.0")) }
    var body: some Scene { WindowGroup { HomeScreen().trooperView("Home") } }
}

CheckoutScreen().trooperView("Checkout")
```

### API

```swift
TaskTrooper.identify(id: user.id, traits: ["plan": "pro"])   // opt-in; PII is your responsibility
TaskTrooper.clearUser()
TaskTrooper.track("checkout_started", properties: ["step": "1"], value: 120)
TaskTrooper.trackAction("buy_button")
TaskTrooper.captureError(error, attributes: ["screen": "cart"])
TaskTrooper.startView("Onboarding"); TaskTrooper.stopView("Onboarding")   // manual views
TaskTrooper.flush()
```

All calls are thread-safe, never throw and are no-ops before `start`.

## Configuration (`TrooperConfig`)

| Field | Default | Description |
|---|---|---|
| `dsn` | — | `https://<key>@<host>[:port]/<project_id>`; invalid DSN disables the SDK |
| `app` | — | App name shown as the service in the dashboard |
| `version` | `TrooperConfig.bundleVersion` | App version/build (deploy marker, RUM `resource.version`): `"<CFBundleShortVersionString> (<CFBundleVersion>)"`. dSYMs must be uploaded under the same string |
| `environment` | `production` | |
| `sampleRate` | `1.0` | Fraction of sessions recorded (decided per session) |
| `tracePropagationTargets` | `[]` | Hosts that get `traceparent`: `"api.shop.com"` (host + subdomains), `"*"`, or a regex starting with `^` |
| `trackViews` | `true` | UIKit view controller swizzling |
| `trackNetwork` | `true` | URLSession RESOURCE events + `traceparent` / `baggage: tt.svc=<app>` |
| `trackCrashes` | `true` | Signal + NSException handlers |
| `trackHangs` / `hangThreshold` | `true` / `2` s | Main-thread watchdog → ERROR `AppHang`, source `anr` |
| `trackFrames` | `true` | `SLOW_FRAMES` / `FROZEN_FRAMES` counts per view (`attributes.total_frames`) |
| `trackAppStart` | `true` | APP_START cold/warm/hot |
| `metricKit` | `false` | Forward MetricKit crash/hang diagnostics (iOS 14+, attribute `metrickit=true`) |
| `ignoredViewControllers` | `[]` | Class names never tracked |
| `flushInterval` / `maxBatchSize` | `5` s / `100` | Batching |
| `maxQueueBytes` | 5 MB | Offline disk queue cap (oldest dropped first) |
| `beforeSend` | `nil` | Mask or drop (`nil`) any event |
| `resourceAttributes` | `[:]` | Extra keys on the batch resource |
| `debug` | `false` | Log SDK internals with `NSLog` |

## How it works

- **Delivery:** events are buffered on a serial queue and written as gzip batches to
  `Application Support/TaskTrooper/queue` (excluded from backup), then uploaded oldest first. 2xx and 4xx (≠429) delete
  the batch; 429/5xx/network errors keep it and back off 1 s → 60 s with jitter (`Retry-After` honoured). Uploads pause
  while offline (`NWPathMonitor`). On `didEnterBackground` the current view ends and everything is flushed inside a
  `beginBackgroundTask`.
- **Network:** `-[NSURLSessionTask resume]` is swizzled on the concrete task classes and task `state` is observed
  (KVO) until completion. This covers data/upload/download tasks, completion handlers, delegates and async/await
  without re-issuing requests (unlike `URLProtocol`, which also breaks background sessions, auth challenges and
  metrics). `traceparent` is added through the task's current request before it starts. Duration is resume →
  completion. URLs are sent without query values. Requests carrying `x-trooper-key` (the SDK's own) are skipped.
- **App start:** `cold` = process start (`sysctl` `kinfo_proc`) → first `CADisplayLink` frame after
  `didBecomeActive` (`attributes.start_reference=process`); `warm` = iOS pre-warmed launch, measured from SDK start
  (`sdk_init`); `hot` = `willEnterForeground` → first frame (`foreground`). Starts > 30 s are discarded.
- **Crashes:** handlers for SIGABRT, SIGSEGV, SIGBUS, SIGILL, SIGTRAP, SIGFPE (alternate stack, previous handlers
  chained) and `NSSetUncaughtExceptionHandler` write a small text report with no malloc/locks/printf
  (pre-opened fd, static buffers, `vm_read_overwrite` frame-pointer walk). The crashed thread comes from the signal
  context; every other thread (up to 64 × 64 frames) is captured with `task_threads` → `thread_suspend` →
  `thread_get_state` → frame-pointer walk, then resumed before the previous handler runs. The
  `__DATA,__crash_info` annotations (`crashreporter_annotations_t.message/message2`) of all loaded images are read
  too: that is where Swift's `fatalError` / `preconditionFailure` / `Array index out of range` text
  (libswiftCore) and `abort()` / libdispatch / libobjc messages live; the section addresses are resolved at image
  load time, so the handler only reads memory. The report is sent on the next launch as ERROR `is_crash=true`,
  source `crash`, `message` = the Swift/annotation message (or NSException reason, or signal + fault address),
  attributes `crash.signal`, `crash.threads`, `crash.info`; attributed to the crashed session/view.
- **Hangs:** a background watchdog pings the main queue every 250 ms. When a ping has waited ≥ `hangThreshold`,
  the watchdog suspends the main thread for a few µs, reads its registers (`thread_get_state`), walks the frame
  pointers and resumes it; the hang is reported when the main thread recovers (ERROR `AppHang`, source `anr`) with
  that stack in the same Apple format + `Binary Images` (paused in background and when a debugger is attached).

## dSYMs / symbolication

Crash and hang stacks are unsymbolicated addresses in Apple format — `Thread 0 Crashed:` (crashed thread),
`Thread N:` (others, `Thread N name:  main` for the main thread), frames as
`<idx> <image> 0x<pc> 0x<load> + <offset>` — plus a `Binary Images:` section
(`0x<load> - 0x<end> <name> <arch> <UUID> <path>`). `services/symbolicator` symbolicates them with the dSYM whose
UUID matches, looked up by `app` + `release` (= `resource.version`).

Upload dSYMs for every release build (Build Settings → *Debug Information Format* = *DWARF with dSYM File*) with
`Scripts/upload-dsyms.sh` (`POST <TROOPER_API_URL>/v1/artifacts`, multipart `kind=dsym`, `app`, `release`, one zip
per `.dSYM`, `Authorization: Bearer <TROOPER_API_KEY>` — the `trooper-cli upload-dsym` contract):

```bash
# Xcode: target → Build Phases → New Run Script Phase (after "Embed"; input file
# ${DWARF_DSYM_FOLDER_PATH}/${DWARF_DSYM_FILE_NAME}). Uploads everything in DWARF_DSYM_FOLDER_PATH.
export TROOPER_API_URL=https://symbols.example.com TROOPER_API_KEY=... TROOPER_APP=shop-ios
[ "$CONFIGURATION" = "Release" ] && "${SRCROOT}/../sdks/ios/Scripts/upload-dsyms.sh"

# fastlane / CI: pass .dSYM bundles, folders or .zip archives (e.g. App Store Connect's dSYMs zip)
sh("TROOPER_APP=shop-ios TROOPER_RELEASE='1.4.0 (812)' ../sdks/ios/Scripts/upload-dsyms.sh #{lane_context[SharedValues::DSYM_OUTPUT_PATH]}")
```

`release` defaults to `"<CFBundleShortVersionString> (<CFBundleVersion>)"` of the built Info.plist (or
`TROOPER_INFO_PLIST`, or `MARKETING_VERSION` / `CURRENT_PROJECT_VERSION`) — identical to `TrooperConfig.bundleVersion`,
the default `version`. If you pass a custom `version` to `TrooperConfig`, set `TROOPER_RELEASE` to the same string.
Errors are printed as Xcode warnings and never fail the build unless `TROOPER_STRICT=1`; `TROOPER_SKIP=1` disables it.
With Bitcode-free App Store builds the local dSYMs are the final ones; no download from App Store Connect is needed.

## App extensions

The SDK uses extension-safe API only (CocoaPods sets `APPLICATION_EXTENSION_API_ONLY = YES`; the package builds with
it, verified in CI via `xcodebuild … APPLICATION_EXTENSION_API_ONLY=YES build`), so it can be linked into widgets,
share / action / notification-service extensions, etc. Inside an extension (`Bundle.main.bundlePath` ends in
`.appex`) `UIApplication.shared` is never touched: lifecycle comes from `NSExtensionHost*` notifications, the
background flush uses `ProcessInfo.performExpiringActivity` (also used in apps), and the extension is assumed to be
active at start. Use a distinct `app` (e.g. `shop-ios-widget`) or `resourceAttributes` to tell it apart from the app.
Extensions usually have no view controllers worth tracking — set `trackViews = false` or name screens manually.

## Deployment target

- **CocoaPods: iOS / tvOS 15.0.** Xcode 27 no longer builds for iOS < 15, so `pod lib lint` fails with 13.0.
  CocoaPods users on Xcode 27 cannot target lower anyway.
- **SPM: unchanged at iOS / tvOS 13** (`Package.swift`). Raising it would make package resolution fail for apps
  that still declare 13/14 with older Xcode versions; with Xcode 27 the effective minimum is raised to 15
  automatically. No 13/14-only API paths were removed, but they are only exercised by older Xcodes.

## Hybrid apps (Flutter / React Native)

`TaskTrooper.hybridContext()`, `TaskTrooper.setHybridContext(_:)` and `TaskTrooper.processStartTimeMs()` implement
`getContext` / `setContext` / `getProcessStartTime` of the `dev.tasktrooper.telemetry/rum` channel
(contracts.md §9). The Flutter plugin / RN native module registers the channel and forwards to these methods.

## Limitations

- **Async-signal-safety of the all-threads capture:** `task_threads` / `thread_suspend` / `thread_get_state` are
  Mach traps, not on the POSIX async-signal-safe list. `task_threads` allocates the port array with `vm_allocate`
  (kernel, no malloc lock) — the same approach PLCrashReporter and KSCrash use. If any call fails the report still
  has the crashed thread. At most 64 other threads × 64 frames are written; thread names other than `main` are not
  read (`pthread_getname_np` is not signal-safe).
- Stacks are walked via frame pointers (always present on arm64 Apple ABIs); frames of code compiled with
  `-fomit-frame-pointer` (x86_64 simulator only) may be skipped. No DWARF/compact-unwind unwinding.
- `crash_info` messages are read from images' `__DATA,__crash_info`; messages Swift writes only to stderr (some
  older runtimes) are not captured. The simulator's permanent `CoreSimulator …` annotation is ignored.
- The hang stack is one sample taken when the threshold is crossed, not a profile of the whole hang (MetricKit
  `metricKit = true` additionally provides Apple's hang call trees).
- Stack overflow on a secondary thread is reported only when that thread has an alternate signal stack (only the main
  thread gets one).

## Development

```bash
swift build && swift test                                    # macOS: platform-independent parts
xcodebuild -scheme TaskTrooper-Package -destination 'platform=iOS Simulator,name=iPhone 17 Pro' test
xcodebuild -scheme TaskTrooper-Package -destination 'generic/platform=iOS Simulator' APPLICATION_EXTENSION_API_ONLY=YES build
# pod lib lint expects the repo layout (paths are relative to the repo root): run it from the repository root,
# or from a temp dir containing TaskTrooper.podspec + sdks/ios → `pod lib lint --allow-warnings --platforms=ios`
cd Examples/TrooperSample && xcodebuild -scheme TrooperSample -destination 'generic/platform=iOS Simulator' build
```
