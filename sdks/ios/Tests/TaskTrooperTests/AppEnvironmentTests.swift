import XCTest
@testable import TaskTrooper

final class AppEnvironmentTests: XCTestCase {
    func testExtensionDetection() {
        XCTAssertTrue(AppEnvironment.isExtension(bundlePath: "/private/var/containers/Bundle/Application/X/Shop.app/PlugIns/ShareExt.appex"))
        XCTAssertTrue(AppEnvironment.isExtension(bundlePath: "/a/Widget.appex/"))
        XCTAssertFalse(AppEnvironment.isExtension(bundlePath: "/private/var/containers/Bundle/Application/X/Shop.app"))
        XCTAssertFalse(AppEnvironment.isExtension(bundlePath: "/a/appex.app"))
        XCTAssertFalse(AppEnvironment.isAppExtension, "test host is not an extension")
    }

    func testBundleVersionFormat() {
        XCTAssertEqual(TrooperConfig.bundleVersion(info: ["CFBundleShortVersionString": "1.4.0", "CFBundleVersion": "812"]), "1.4.0 (812)")
        XCTAssertEqual(TrooperConfig.bundleVersion(info: ["CFBundleShortVersionString": "1.4.0"]), "1.4.0")
        XCTAssertEqual(TrooperConfig.bundleVersion(info: ["CFBundleVersion": "812"]), "812")
        XCTAssertEqual(TrooperConfig.bundleVersion(info: [:]), "")
    }
}
