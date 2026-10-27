import XCTest
@testable import TaskTrooperReplay

final class CaptureThrottleTests: XCTestCase {
    func testFirstIdleCapturesThenThrottlesWithOneTrailing() {
        var t = CaptureThrottle()
        XCTAssertEqual(t.runLoopIdle(now: 10), .capture)
        t.didCapture(now: 10, cost: 0.001, truncated: false)
        XCTAssertEqual(t.runLoopIdle(now: 10.125), .scheduleTrailing(0.375))
        XCTAssertEqual(t.runLoopIdle(now: 10.2), .none, "only one trailing capture is pending")
        XCTAssertTrue(t.trailingFired())
        t.didCapture(now: 10.5, cost: 0.001, truncated: false)
        XCTAssertEqual(t.runLoopIdle(now: 10.5), .none, "the iteration of our own trailing capture is not a UI change")
    }

    /// Regression: the trailing timer (with leeway) fired after a run loop wake-up had already captured at the due
    /// time, so every period captured twice ~15 ms apart (seen live on the iOS 26 simulator).
    func testTrailingIsSkippedWhenACaptureRanAfterScheduling() {
        var t = CaptureThrottle()
        t.didCapture(now: 10, cost: 0.001, truncated: false)
        XCTAssertEqual(t.runLoopIdle(now: 10.25), .scheduleTrailing(0.25))
        XCTAssertEqual(t.runLoopIdle(now: 10.5), .capture) // wake-up reaches the due time before the timer
        t.didCapture(now: 10.5, cost: 0.001, truncated: false)
        XCTAssertFalse(t.trailingFired())
        XCTAssertEqual(t.runLoopIdle(now: 10.625), .scheduleTrailing(0.375), "a new trailing can be scheduled afterwards")
    }

    func testIntervalFollowsCaptureCost() {
        var t = CaptureThrottle()
        t.didCapture(now: 0, cost: 0.002, truncated: false)
        XCTAssertEqual(t.interval, 0.5)
        t.didCapture(now: 0, cost: 0.012, truncated: false)
        XCTAssertEqual(t.interval, 1.2, accuracy: 1e-9)
        t.didCapture(now: 0, cost: 0.2, truncated: false)
        XCTAssertEqual(t.interval, 5)
        t.didCapture(now: 0, cost: 0.001, truncated: true)
        XCTAssertEqual(t.interval, 2)
    }

    func testOwnWakeSwallowsOneIdle() {
        var t = CaptureThrottle()
        t.markOwnWake()
        XCTAssertEqual(t.runLoopIdle(now: 1), .none)
        XCTAssertEqual(t.runLoopIdle(now: 1), .capture)
    }
}
