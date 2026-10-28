import XCTest
@_spi(TrooperReplay) @testable import TaskTrooper

final class ReplayBridgeTests: XCTestCase {
    private func makeCore(sampleRate: Double = 1, clock: ManualClock = ManualClock()) -> TrooperCore {
        var config = TrooperConfig(dsn: "https://key1@ingest.test/1", app: "shop-ios", version: "2.0", environment: "test", sampleRate: sampleRate)
        config.trackCrashes = false
        var env = TrooperCore.Environment()
        env.clock = clock.clock
        env.defaults = freshDefaults()
        env.storageDirectory = tempDirectory()
        env.urlSession = StubProtocol.session()
        env.startTimer = false
        env.random = { 0.5 }
        return TrooperCore(config: config, environment: env)!
    }

    func testErrorObserversSeeErrorsOfSampledSessionsOnly() {
        let c = makeCore()
        let seen = Locked<[Int64]>([])
        c.errorObservers.withLock { $0.append { ts in seen.withLock { $0.append(ts) } } }
        c.submit(.error, ts: 42) { $0.error = RumErrorPayload(type: "E", message: "m", stack: nil, source: "source", isCrash: false, thread: nil, fingerprint: nil) }
        c.submit(.custom) { $0.custom = RumCustomPayload(name: "x") }
        var report = CrashReport()
        report.tsMs = 7
        report.sessionId = "old"
        c.emitCrash(report) // previous process: not an error of the replayed session
        c.queue.sync {}
        c.queue.sync {}
        XCTAssertEqual(seen.current, [42])

        let unsampled = makeCore(sampleRate: 0)
        let calls = Locked(0)
        unsampled.errorObservers.withLock { $0.append { _ in calls.withLock { $0 += 1 } } }
        unsampled.submit(.error) { _ in }
        unsampled.queue.sync {}
        XCTAssertEqual(calls.current, 0)
    }

    func testReplaySessionRotatesExpiredSessionWithoutExtending() {
        let clock = ManualClock()
        let c = makeCore(clock: clock)
        c.prime()
        let first = c.queue.sync { c.replaySession() }
        clock.advance(seconds: 10 * 60)
        let same = c.queue.sync { c.replaySession() }
        XCTAssertEqual(first.id, same.id)
        XCTAssertEqual(same.lastActivityMs, first.lastActivityMs, "replay must not extend the session")
        clock.advance(seconds: 25 * 60) // 35 min idle
        let rotated = c.queue.sync { c.replaySession() }
        XCTAssertNotEqual(rotated.id, first.id)
    }

    func testBridgeIsInertBeforeStart() {
        TaskTrooper.resetForTesting()
        XCTAssertNil(TrooperReplayBridge.context())
        XCTAssertNil(TrooperReplayBridge.ingest)
        XCTAssertFalse(TrooperReplayBridge.addErrorObserver { _ in })
        XCTAssertEqual(TrooperReplayBridge.classify(statusCode: 413, retryAfter: nil, hasError: false), .drop)
        XCTAssertEqual(TrooperReplayBridge.classify(statusCode: 429, retryAfter: "3", hasError: false), .retry(after: 3))
    }
}
