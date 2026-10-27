// swift-tools-version:5.9
import PackageDescription

let strictConcurrency: [SwiftSetting] = [.enableExperimentalFeature("StrictConcurrency")]

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
            path: "Sources/TaskTrooperCrashC",
            linkerSettings: [.linkedLibrary("z")]
        ),
        .target(
            name: "TaskTrooper",
            dependencies: ["TaskTrooperCrashC"],
            path: "Sources/TaskTrooper",
            swiftSettings: strictConcurrency
        ),
        .target(
            name: "TaskTrooperSwiftUI",
            dependencies: ["TaskTrooper"],
            path: "Sources/TaskTrooperSwiftUI",
            swiftSettings: strictConcurrency
        ),
        .target(
            name: "TaskTrooperReplay",
            dependencies: ["TaskTrooper"],
            path: "Sources/TaskTrooperReplay",
            swiftSettings: strictConcurrency
        ),
        .testTarget(
            name: "TaskTrooperReplayTests",
            dependencies: ["TaskTrooperReplay", "TaskTrooper"],
            path: "Tests/TaskTrooperReplayTests",
            swiftSettings: strictConcurrency
        ),
        .testTarget(
            name: "TaskTrooperTests",
            dependencies: ["TaskTrooper", "TaskTrooperCrashC"],
            path: "Tests/TaskTrooperTests",
            swiftSettings: strictConcurrency
        ),
    ]
)
