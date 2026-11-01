# iOS SDK — testing

Cross-cutting rules: [`../../../.ai/testing.md`](../../../.ai/testing.md). Code rules: [coding-standards.md](coding-standards.md).

## Gates — what actually runs

- **Not in PR CI.** `.github/workflows/ci.yml` and `make check` do not build or test `sdks/ios`.
- **Release only:** `.github/workflows/release-ios.yml` (tag `ios-vX.Y.Z`, `macos-latest`) runs `swift build` and
  `swift test` against the **repo-root** `Package.swift` (macOS host), then `pod trunk push` (which lints the pod).
  Simulator tests, the extension-safety build and the sample app are **not** run by any workflow.
- So: run the local commands below before every PR that touches `sdks/ios` or the root `Package.swift`.

## Run locally

```bash
cd sdks/ios
swift build && swift test                       # macOS host: platform-independent logic (fast loop)
swift test --filter TaskTrooperTests.TransportTests
xcodebuild -scheme TaskTrooper-Package -destination 'platform=iOS Simulator,name=iPhone 17 Pro' test   # UIKit paths
xcodebuild -scheme TaskTrooper-Package -destination 'generic/platform=iOS Simulator' \
  APPLICATION_EXTENSION_API_ONLY=YES build      # extension safety

# from the repo root (what the release gate runs + the published manifest)
swift build && swift test
cp sdks/ios/TaskTrooper.podspec . && pod lib lint TaskTrooper.podspec --allow-warnings --platforms=ios; rm TaskTrooper.podspec
```

Run the simulator `xcodebuild … test` when touching UIKit integration, replay mapping, hang detection or anything
behind `#if canImport(UIKit)` — `swift test` on macOS compiles those paths out.

## Framework and layout

- **XCTest** only (`XCTestCase`; no swift-testing yet). `@testable import TaskTrooper`.
- `Tests/TaskTrooperTests/`: batching, transport, session, DSN, views, trace + network, crash reports, hybrid
  context, replay bridge, app environment, `ProtoSchemaTests`.
- `Tests/TaskTrooperReplayTests/`: wireframe, UIKit mapper, engine, capture throttle, segment uploader, SwiftUI
  layer fallback, stop.
- UI test: `Examples/TrooperSample/UITests/ReplayUITests.swift` (generate the project with `xcodegen generate` in
  `Examples/TrooperSample`; the `.xcodeproj` is git-ignored).

## Fakes and helpers (`Tests/TaskTrooperTests/StubProtocol.swift`)

- `StubProtocol` — `URLProtocol` stub; `StubProtocol.session()` gives a `URLSession` that records requests and
  answers from `StubProtocol.reset { req in (status, headers, body, error) }`.
- `ManualClock` — controllable `TrooperClock`; `freshDefaults()` / `tempDirectory()` isolate `UserDefaults` and disk.
- Build cores through `TrooperCore.Environment` (see `makeCore` in `CoreIntegrationTests.swift`): inject clock,
  defaults, storage dir, stub session, `startTimer = false`, deterministic `random`.
- Wait with `XCTestExpectation` (e.g. `core.flush { exp.fulfill() }`), never `sleep`.

## Rules

- Payload changes: `ProtoSchemaTests` checks emitted JSON field/enum names against `proto/rum/v1/rum.proto`.
  It **skips** (`XCTSkip`) when the proto file is not found — make sure it actually ran (it does from the repo).
  Update the proto first, then `Model/RumModels.swift`.
- Test logic on a `TrooperCore` built from `Environment`, not through the global `TaskTrooper.start` (global state
  leaks between tests; undo swizzles/handlers in `tearDown`, as `TraceAndNetworkTests` / `CrashReportTests` do).
  There is no "public API is a no-op before `start`" test yet (Android has one) — add it when touching `TaskTrooper.swift`.
- Transport: cover 2xx, 4xx drop, 429 + `Retry-After`, 5xx backoff, offline pause, queue cap.
- Crash: parsing of the on-disk report is tested (`CrashReportTests`); signal-path changes must keep the
  previous-handler chaining — verify manually in the sample app (no automated real-crash harness on iOS).
- Replay mapper: assert text fields / secure entry stay masked with `maskAllText: false`.
- Naming: `<Subject>Tests.swift`, `func test<Behaviour>()`.
