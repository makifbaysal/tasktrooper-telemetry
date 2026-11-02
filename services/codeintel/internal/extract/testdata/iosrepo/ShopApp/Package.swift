// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "ShopApp",
    platforms: [.iOS(.v17)],
    dependencies: [.package(url: "https://github.com/firebase/firebase-ios-sdk", from: "10.0.0")],
    targets: [.executableTarget(name: "ShopApp"), .testTarget(name: "ShopAppTests", dependencies: ["ShopApp"])]
)
