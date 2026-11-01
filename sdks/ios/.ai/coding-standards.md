# iOS SDK — coding standards

Scope: `sdks/ios` (SwiftPM targets `TaskTrooper`, `TaskTrooperCrashC`, `TaskTrooperSwiftUI`, `TaskTrooperReplay`;
CocoaPods `TaskTrooper.podspec` subspecs `Core` / `Replay` / `SwiftUI`; `Scripts/upload-dsyms.sh`;
`Examples/TrooperSample`).
Cross-cutting rules: [`../../../.ai/coding-standards.md`](../../../.ai/coding-standards.md).
Design and wire format: [`../../../.ai/sdk.md`](../../../.ai/sdk.md), [`../../../.ai/contracts.md`](../../../.ai/contracts.md)
(§2 DSN, §9 hybrid channel, §23 replay), RUM schema [`../../../proto/rum/v1/rum.proto`](../../../proto/rum/v1/rum.proto).
User-facing behaviour: [`../README.md`](../README.md) — keep it in sync.

## Toolchain and manifests

| Item | Value | Where |
|---|---|---|
| Swift tools | 5.9; podspec `swift_versions ['5.9', '6.0']` | `Package.swift`, podspec |
| Concurrency | `StrictConcurrency` enabled on every Swift target | `Package.swift` |
| Platforms (SPM) | iOS 13, tvOS 13, macOS 11 (macOS only for `swift test`) | `Package.swift` |
| Platforms (CocoaPods) | iOS / tvOS 15.0 (Xcode 27 can't lint below 15) | podspec |
| Linter / formatter | none configured (no SwiftLint / swift-format) — match surrounding style | — |

- **Two `Package.swift` files must stay in sync:** repo-root `Package.swift` (published, paths prefixed `sdks/ios/`)
  and `sdks/ios/Package.swift` (local dev). Adding a target/file pattern → edit both, plus the podspec subspec.
- **Version lives in three places:** `s.version` (podspec), `sdkVersion` in `Platform/DeviceInfo.swift` and in
  `TaskTrooperReplay/TrooperReplay.swift`. Bump together (README § Releasing).
- Don't raise the SPM deployment target (breaks resolution for apps declaring 13/14).

## Source layout (`Sources/TaskTrooper`)

`Core/` (config, session, batcher, `TrooperCore`), `Transport/` (disk queue, uploader, backoff, gzip),
`Instrumentation/` (network, crash, hang, frames, trace propagation), `Platform/` (UIKit, reachability, device,
MetricKit), `Model/` (`RumModels.swift`, Codable mirror of rum.proto), `Util/` (`Locked`, `TrooperClock`, logging,
URL sanitizer). Signal-handler code is C/ObjC in `TaskTrooperCrashC` only.

## Public API style

- Namespace `public enum TaskTrooper` with static methods; config is a `public struct TrooperConfig` with
  defaults (`dsn`, `app` required).
- `TaskTrooper.start(_:)` is idempotent: a second call logs and returns. Invalid DSN/storage → SDK disabled, no throw.
- Every public method: thread-safe, no-op before `start`, never throws (`core.current?.…`).
- Testability seam: internal `start(_:environment:installUI:)` with `TrooperCore.Environment` (clock, defaults,
  storage dir, `URLSession`, backoff, scheduler, random). New dependencies go into `Environment`, not globals.
- Optional products (`TaskTrooperReplay`, `TaskTrooperSwiftUI`) depend on `TaskTrooper` only through internal bridge
  types (`Core/ReplayBridge.swift`); core never imports them.

## Never crash the host app

- No `try!`, `fatalError`, `precondition`, force unwrap or force cast in `Sources/`. Use `guard … else { return }`
  and failable inits (`TrooperCore(config:environment:)` returns `nil`).
- `beforeSend` and other user closures may misbehave — keep them off the main thread and out of locks.
- Internal failures are logged through `TrooperLog.debug` (only with `config.debug`). There is no internal error
  counter on iOS yet (Android/Flutter have one) — if you add one, follow `../../../.ai/sdk.md`.
- Crash handlers (`TTCrash.c`, `TTException.m`): async-signal-safe — no malloc, locks, ObjC/Swift runtime calls or
  `printf` in the signal path; pre-opened fd, static buffers. **Always chain** the previous signal handler and the
  previous `NSUncaughtExceptionHandler`. Report is written now, sent on next launch.
- Hang detector: never block the main thread for more than a sample (suspend → read registers → resume).

## Threading and concurrency

- All core state is confined to the serial queue `dev.tasktrooper.core` (`.utility`); replay uses
  `dev.tasktrooper.replay`. Cross-thread state uses `Locked<T>` (NSLock box), not ad-hoc locks.
- UIKit work is `@MainActor` (`UIKitIntegration`); from a background thread hop with `DispatchQueue.main.async`.
- Closures crossing queues are `@Sendable`. Use `@unchecked Sendable` only for lock-protected classes and say why.
- No disk or network I/O on the main thread. Background flush uses `beginBackgroundTask` /
  `ProcessInfo.performExpiringActivity`.
- Replay capture runs on the main thread with a budget (1500 nodes / 12 ms, ≤ 2 fps). Don't add work per node.

## Extension safety and platforms

- Core is `APPLICATION_EXTENSION_API_ONLY` (podspec). Never reference `UIApplication.shared` in `TaskTrooper`;
  branch on `AppEnvironment` (`.appex`) instead. Replay may use `UIApplication` but must be a no-op in extensions/tvOS.
- Guard UIKit/SwiftUI/MetricKit code with `#if canImport(UIKit)` / `#if os(iOS)` so `swift build` on macOS works.

## Delivery and persistence

- Gzip batches in `Application Support/TaskTrooper/queue` (excluded from backup), 5 MB cap, oldest dropped.
- 2xx and 4xx (≠ 429) delete; 429/5xx/network keep + backoff 1 s → 60 s with jitter, `Retry-After` honoured;
  uploads pause while offline (`NWPathMonitor`).
- Network instrumentation swizzles `NSURLSessionTask.resume` + KVO; never re-issue requests, never use `URLProtocol`
  for instrumentation. Skip requests carrying `x-trooper-key`.

## Privacy

- No headers, bodies or query values (`Util/URLSanitizer.swift`). User data only via `identify`.
- Replay: wireframes only. All text masked by default; `UITextField` / `UITextView` / secure entry always masked
  and their subviews never read; `WKWebView` is a black box; keyboard windows skipped. New mappers default to masked.

## Dependencies

None beyond Apple frameworks (Foundation, UIKit, SwiftUI, Network, weak MetricKit) and `libz`. Keep it that way.

## Commits

`feat(sdk-ios): …`, `fix(sdk-ios): …` (`sdk-mobile` for cross-platform changes). English everywhere.
