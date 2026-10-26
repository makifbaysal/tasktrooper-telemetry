// swift-tools-version:5.9
import PackageDescription

let strictConcurrency: [SwiftSetting] = [.enableExperimentalFeature("StrictConcurrency")]

let package = Package(
    name: "TaskTrooper",
    platforms: [.iOS(.v13), .tvOS(.v13), .macOS(.v11)],
    products: [
        .library(name: "TaskTrooper", targets: ["TaskTrooper"]),
        .library(name: "TaskTrooperSwiftUI", targets: ["TaskTrooperSwiftUI"]),
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
        .testTarget(
            name: "TaskTrooperTests",
            dependencies: ["TaskTrooper", "TaskTrooperCrashC"],
            path: "Tests/TaskTrooperTests",
            swiftSettings: strictConcurrency
        ),
    ]
)
