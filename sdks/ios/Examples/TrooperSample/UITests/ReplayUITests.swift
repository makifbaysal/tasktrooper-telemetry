import XCTest

/// Drives the sample through the session replay screens (SwiftUI home, UIKit form with secure field, alert,
/// table scroll, SwiftUI list, rotation) against a local ingest-gw. Assertions on the recorded wireframes are
/// made on the backend side (`replay_segments` / `GET /api/v1/replays`); here we only exercise the UI.
/// `TROOPER_REPLAY_OFF=1` (env of the test run) launches the app with replay disabled for hitch comparison.
final class ReplayUITests: XCTestCase {
    override func setUp() {
        continueAfterFailure = true
    }

    func testDriveReplayScreens() throws {
        let app = XCUIApplication()
        if ProcessInfo.processInfo.environment["TROOPER_REPLAY_OFF"] == "1" { app.launchArguments.append("-trooperReplayOff") }
        app.launch()
        XCUIDevice.shared.orientation = .portrait
        pause(1.5)

        // SwiftUI home: type into the SecureField.
        let pw = app.secureTextFields["homePassword"]
        if pw.waitForExistence(timeout: 5) {
            pw.tap(); pw.typeText("hunter2!")
            pause(1)
        }
        app.buttons["Log in as demo user"].tap()
        pause(1)

        // UIKit form.
        app.buttons["openForm"].tap()
        let email = app.textFields["email"]
        XCTAssertTrue(email.waitForExistence(timeout: 5))
        email.tap(); email.typeText("jane@example.com")
        pause(0.8)
        let password = app.secureTextFields["password"]
        password.tap(); password.typeText("s3cret-pass")
        pause(0.8)
        app.switches["remember"].tap()
        pause(0.8)

        // Alert (separate window) with a text field.
        app.buttons["showAlert"].tap()
        let coupon = app.textFields["coupon"]
        if coupon.waitForExistence(timeout: 5) { coupon.typeText("SAVE10") }
        pause(1.2)
        app.alerts.buttons["Apply"].tap()
        pause(0.8)

        // Table scroll.
        let table = app.tables["table"]
        table.swipeUp(); pause(0.7)
        table.swipeUp(); pause(0.7)
        table.swipeDown(); pause(0.7)

        // Rotate.
        XCUIDevice.shared.orientation = .landscapeLeft
        pause(1.5)
        XCUIDevice.shared.orientation = .portrait
        pause(1.5)

        app.buttons["closeForm"].tap()
        pause(1)

        // SwiftUI list.
        app.buttons["openList"].tap()
        pause(1.5)
        let search = app.textFields["search"]
        if search.waitForExistence(timeout: 5) { search.tap(); search.typeText("order 12"); pause(1) }
        app.switches.firstMatch.tap()
        pause(0.8)
        for _ in 0..<3 { app.swipeUp(); pause(0.6) }
        app.swipeDown(velocity: .fast)
        pause(1)

        // Background → the SDK flushes the open segment.
        XCUIDevice.shared.press(.home)
        pause(4)
    }

    /// Hitch measurement while scrolling the UIKit table (compare with `TROOPER_REPLAY_OFF=1`).
    func testScrollHitches() throws {
        let app = XCUIApplication()
        if ProcessInfo.processInfo.environment["TROOPER_REPLAY_OFF"] == "1" { app.launchArguments.append("-trooperReplayOff") }
        app.launch()
        XCUIDevice.shared.orientation = .portrait
        app.buttons["openForm"].tap()
        let table = app.tables["table"]
        XCTAssertTrue(table.waitForExistence(timeout: 5))
        let options = XCTMeasureOptions()
        options.iterationCount = 5
        measure(metrics: [XCTOSSignpostMetric.scrollDecelerationMetric], options: options) {
            table.swipeUp(velocity: .fast)
            table.swipeDown(velocity: .fast)
        }
    }

    private func pause(_ s: TimeInterval) { Thread.sleep(forTimeInterval: s) }
}
