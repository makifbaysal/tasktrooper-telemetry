import XCTest
@testable import TaskTrooper

final class BatchingTests: XCTestCase {
    private func event(_ i: Int) -> RumEvent {
        RumEvent(type: .custom, tsMs: Int64(i), sessionId: "s", anonymousId: "a")
    }

    func testFlushesAtHundredEvents() {
        var batches: [[RumEvent]] = []
        let b = EventBatcher(maxBatchSize: 100, maxAge: 5) { batches.append($0) }
        for i in 0..<99 { b.add(event(i), nowMs: 0) }
        XCTAssertTrue(batches.isEmpty)
        b.add(event(99), nowMs: 0)
        XCTAssertEqual(batches.count, 1)
        XCTAssertEqual(batches[0].count, 100)
        XCTAssertTrue(b.buffer.isEmpty)
    }

    func testFlushesAfterFiveSecondsFromFirstEvent() {
        var batches: [[RumEvent]] = []
        let b = EventBatcher(maxBatchSize: 100, maxAge: 5) { batches.append($0) }
        b.add(event(1), nowMs: 10_000)
        b.add(event(2), nowMs: 13_000)
        XCTAssertFalse(b.tick(nowMs: 14_999))
        XCTAssertTrue(b.tick(nowMs: 15_000))
        XCTAssertEqual(batches.first?.count, 2)
        XCTAssertFalse(b.tick(nowMs: 30_000), "empty buffer never flushes")
    }

    func testManualFlush() {
        var count = 0
        let b = EventBatcher { count += $0.count }
        b.flush()
        XCTAssertEqual(count, 0)
        b.add(event(1), nowMs: 0)
        b.flush()
        XCTAssertEqual(count, 1)
    }
}
