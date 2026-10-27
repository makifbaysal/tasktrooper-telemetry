import XCTest
@_spi(TrooperReplay) @testable import TaskTrooper

/// Hybrid apps (React Native / Flutter + native SDK, contracts.md §9): the session adopted via `setContext` is external.
final class HybridContextTests: XCTestCase {
    private func makeCore(sampleRate: Double = 1, clock: ManualClock = ManualClock()) -> TrooperCore {
        var config = TrooperConfig(dsn: "https://key1@ingest.test/1", app: "shop-rn", version: "2.0", environment: "test", sampleRate: sampleRate)
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

    private func sentEvents(_ c: TrooperCore) throws -> [RumEvent] {
        let exp = expectation(description: "flush")
        c.flush { exp.fulfill() }
        wait(for: [exp], timeout: 5)
        return try StubProtocol.captured.current.flatMap { cap -> [RumEvent] in
            try JSONDecoder().decode(RumBatch.self, from: XCTUnwrap(Gzip.decompress(cap.body))).events
        }
    }

    private func custom(_ c: TrooperCore, _ name: String) {
        c.submit(.custom) { $0.custom = RumCustomPayload(name: name) }
        c.queue.sync {}
    }

    func testDictionaryParsesSampled() {
        let base: [String: Any] = ["session_id": "js-1", "anonymous_id": "a1", "view_name": "Cart"]
        XCTAssertNil(TrooperHybridContext(dictionary: base)?.sampled)
        var d = base; d["sampled"] = false
        XCTAssertEqual(TrooperHybridContext(dictionary: d)?.sampled, false)
        d["sampled"] = NSNumber(value: true) // RN / Flutter channels deliver NSNumber
        XCTAssertEqual(TrooperHybridContext(dictionary: d)?.sampled, true)
        d["sampled"] = "yes"
        XCTAssertNil(TrooperHybridContext(dictionary: d)?.sampled)
        XCTAssertNil(TrooperHybridContext(dictionary: ["anonymous_id": "a1"]))
    }

    /// The adopted session is never rotated by native inactivity (30 min) / max duration (4 h) rules, native events
    /// carry it, and native never sends its own SESSION_START for it.
    func testAdoptedSessionIsExternalAndNeverRotated() throws {
        StubProtocol.reset()
        let clock = ManualClock()
        let c = makeCore(clock: clock)
        c.setExternalContext(TrooperHybridContext(sessionId: "js-1", anonymousId: "anon-js", viewId: "v1", viewName: "Cart",
                                                  userId: "u1", sampled: true))
        custom(c, "a")
        clock.advance(seconds: 45 * 60) // > inactivity
        custom(c, "b")
        clock.advance(seconds: 5 * 60 * 60) // > max duration
        custom(c, "c")
        // Re-adopting the same id is not a no-op that lets it expire: still the same external session.
        c.setExternalContext(TrooperHybridContext(sessionId: "js-1", anonymousId: "anon-js", viewName: "Checkout"))
        clock.advance(seconds: 31 * 60)
        custom(c, "d")

        let events = try sentEvents(c)
        XCTAssertEqual(events.map(\.type), [.custom, .custom, .custom, .custom], "no native SESSION_START")
        XCTAssertEqual(Set(events.map(\.sessionId)), ["js-1"])
        XCTAssertEqual(events.first?.anonymousId, "anon-js")
        XCTAssertEqual(events.first?.viewName, "Cart")
        XCTAssertEqual(events.first?.user?.id, "u1")
        XCTAssertEqual(events.last?.viewName, "Checkout")

        XCTAssertEqual(c.nativeContext()?.sessionId, "js-1", "getContext returns the adopted session")
        let replay = c.queue.sync { c.replaySession() }
        XCTAssertEqual(replay.id, "js-1")
    }

    func testSampledFromJSDecidesNativeSampling() throws {
        StubProtocol.reset()
        let c = makeCore(sampleRate: 1)
        c.setExternalContext(TrooperHybridContext(sessionId: "js-off", anonymousId: "a", sampled: false))
        custom(c, "dropped")
        c.queue.sync {}
        XCTAssertFalse(c.sessionSampled.current, "trace propagation follows the adopted decision")
        XCTAssertEqual(c.queue.sync { c.externalContext?.sampled }, false)

        // Native says unsampled (rate 0), JS says sampled: JS wins.
        let unsampled = makeCore(sampleRate: 0)
        unsampled.setExternalContext(TrooperHybridContext(sessionId: "js-on", anonymousId: "a", sampled: true))
        custom(unsampled, "kept")
        let events = try sentEvents(unsampled)
        XCTAssertEqual(events.map { $0.custom?.name }, ["kept"])
        XCTAssertEqual(events.first?.sessionId, "js-on")
    }

    func testWithoutSampledTheDecisionIsFixedPerAdoptedSession() {
        let c = makeCore(sampleRate: 1)
        c.prime()
        c.setExternalContext(TrooperHybridContext(sessionId: "js-1", anonymousId: "a"))
        c.queue.sync {}
        XCTAssertEqual(c.queue.sync { c.externalContext?.sampled }, true)
        c.sessionSampled.set(false) // e.g. a native decision changing later
        c.setExternalContext(TrooperHybridContext(sessionId: "js-1", anonymousId: "a", viewName: "Next"))
        c.queue.sync {}
        XCTAssertEqual(c.queue.sync { c.externalContext?.sampled }, true, "re-adopting keeps the session's decision")
    }

    func testHandingBackRestoresTheNativeSession() throws {
        StubProtocol.reset()
        let c = makeCore()
        c.prime()
        let native = try XCTUnwrap(c.nativeContext()?.sessionId)
        c.setExternalContext(TrooperHybridContext(sessionId: "js-1", anonymousId: "a", sampled: false))
        c.setExternalContext(nil)
        custom(c, "native")
        XCTAssertEqual(c.nativeContext()?.sessionId, native)
        XCTAssertTrue(c.sessionSampled.current)
    }
}
