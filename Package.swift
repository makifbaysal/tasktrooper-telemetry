// swift-tools-version:5.9
// Swift Package Manager entry point for the TaskTrooper iOS/tvOS SDK (sdks/ios).
//
// SwiftPM only resolves packages whose Package.swift is at the repository root and versions them by plain
// semver tags (`0.1.0`), so this manifest points into sdks/ios. Consumers:
//
//   .package(url: "https://github.com/makifbaysal/tasktrooper-telemetry", from: "0.1.0")
//   .product(name: "TaskTrooper", package: "tasktrooper-telemetry")
//
// Keep in sync with sdks/ios/Package.swift (same products/targets, paths prefixed with sdks/ios/), which stays the
// manifest for local development inside sdks/ios (and the Examples/TrooperSample path dependency).
import PackageDescription

let strictConcurrency: [SwiftSetting] = [.enableExperimentalFeature("StrictConcurrency")]
let root = "sdks/ios"

let package = Package(
    name: "TaskTrooper",
    platforms: [.iOS(.v13), .tvOS(.v13), .macOS(.v11)],
    products: [
        .library(name: "TaskTrooper", targets: ["TaskTrooper"]),
        .library(name: "TaskTrooperSwiftUI", targets: ["TaskTrooperSwiftUI"]),
        // Wireframe session replay (contracts.md §23); iOS only, no-op elsewhere.
        .library(name: "TaskTrooperReplay", targets: ["TaskTrooperReplay"]),
    ],
    targets: [
        // Async-signal-safe crash writer, NSException hook, gzip (zlib) and sysctl helpers.
        .target(
            name: "TaskTrooperCrashC",
            path: "\(root)/Sources/TaskTrooperCrashC",
            linkerSettings: [.linkedLibrary("z")]
        ),
        .target(
            name: "TaskTrooper",
            dependencies: ["TaskTrooperCrashC"],
            path: "\(root)/Sources/TaskTrooper",
            swiftSettings: strictConcurrency
        ),
        .target(
            name: "TaskTrooperSwiftUI",
            dependencies: ["TaskTrooper"],
            path: "\(root)/Sources/TaskTrooperSwiftUI",
            swiftSettings: strictConcurrency
        ),
        .target(
            name: "TaskTrooperReplay",
            dependencies: ["TaskTrooper"],
            path: "\(root)/Sources/TaskTrooperReplay",
            swiftSettings: strictConcurrency
        ),
        .testTarget(
            name: "TaskTrooperReplayTests",
            dependencies: ["TaskTrooperReplay", "TaskTrooper"],
            path: "\(root)/Tests/TaskTrooperReplayTests",
            swiftSettings: strictConcurrency
        ),
        .testTarget(
            name: "TaskTrooperTests",
            dependencies: ["TaskTrooper", "TaskTrooperCrashC"],
            path: "\(root)/Tests/TaskTrooperTests",
            swiftSettings: strictConcurrency
        ),
    ]
)
