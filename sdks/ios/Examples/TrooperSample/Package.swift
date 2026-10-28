// swift-tools-version:5.9
// Minimal SwiftUI sample. Compile-check: `xcodebuild -scheme TrooperSample -destination 'generic/platform=iOS Simulator' build`
// To run it on a device/simulator, create an iOS App target in Xcode and add these sources + the TaskTrooper package.
import PackageDescription

let package = Package(
    name: "TrooperSample",
    platforms: [.iOS(.v15), .macOS(.v12)],
    dependencies: [.package(path: "../..")],
    targets: [
        .executableTarget(
            name: "TrooperSample",
            dependencies: [
                .product(name: "TaskTrooper", package: "ios"),
                .product(name: "TaskTrooperSwiftUI", package: "ios"),
                .product(name: "TaskTrooperReplay", package: "ios"),
            ]
        ),
    ]
)
