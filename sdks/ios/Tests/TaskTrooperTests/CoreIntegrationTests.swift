import XCTest
@testable import TaskTrooper

final class CoreIntegrationTests: XCTestCase {
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

    private func flush(_ c: TrooperCore) {
        let exp = expectation(description: "flush")
        c.flush { exp.fulfill() }
        wait(for: [exp], timeout: 5)
    }

    private func sentEvents() throws -> [RumEvent] {
        try StubProtocol.captured.current.flatMap { cap -> [RumEvent] in
            let json = try XCTUnwrap(Gzip.decompress(cap.body))
            return try JSONDecoder().decode(RumBatch.self, from: json).events
        }
    }

    func testEventsFlowToIngest() throws {
        StubProtocol.reset()
        let clock = ManualClock()
        let c = makeCore(clock: clock)
        c.startView(name: "Home", key: "home", loadingTimeMs: 120)
        c.submit(.custom) { $0.custom = RumCustomPayload(name: "checkout_started", props: ["step": "1"], value: nil) }
        c.queue.sync {}
        c.frameStats.withLock { for _ in 0..<10 { $0.record(duration: 0.05) } }
        clock.advance(seconds: 2)
        c.stopView(key: "home")
        flush(c)

        let req = try XCTUnwrap(StubProtocol.captured.current.first)
        XCTAssertEqual(req.request.url?.absoluteString, "https://ingest.test/v1/rum")
        let batch = try JSONDecoder().decode(RumBatch.self, from: Gzip.decompress(req.body)!)
        XCTAssertEqual(batch.resource.platform, .ios)
        XCTAssertEqual(batch.resource.app, "shop-ios")
        XCTAssertEqual(batch.resource.sdk.name, "TaskTrooper-iOS")

        let events = try sentEvents()
        XCTAssertEqual(events.map(\.type), [.sessionStart, .viewStart, .custom, .viewEnd, .vital, .vital])
        guard events.count == 6 else { return }
        XCTAssertEqual(Set(events.map(\.sessionId)).count, 1)
        let start = events[1]
        XCTAssertEqual(start.viewName, "Home")
        XCTAssertEqual(start.view?.loadingTimeMs, 120)
        XCTAssertEqual(start.view?.isInitial, true)
        XCTAssertEqual(events[2].viewId, start.viewId, "events carry the current view")
        XCTAssertEqual(events[3].view?.timeSpentMs, 2000)
        XCTAssertEqual(events[4].vital?.name, "SLOW_FRAMES")
        XCTAssertEqual(events[4].vital?.value, 10)
        XCTAssertEqual(events[4].attributes?["total_frames"], "10")
    }

    func testUnsampledSessionSendsNothing() {
        StubProtocol.reset()
        let c = makeCore(sampleRate: 0)
        c.submit(.custom) { $0.custom = RumCustomPayload(name: "x") }
        flush(c)
        XCTAssertTrue(StubProtocol.captured.current.isEmpty)
        XCTAssertFalse(c.sessionSampled.current)
    }

    func testCrashFromPreviousLaunchKeepsItsSession() throws {
        StubProtocol.reset()
        let c = makeCore()
        var report = CrashReport()
        report.tsMs = 1_699_000_000_000
        report.signal = 11
        report.signalName = "SIGSEGV"
        report.sessionId = "old-session"
        report.viewName = "Cart"
        report.frames = [0x1000]
        report.complete = true
        c.emitCrash(report)
        flush(c)
        let crash = try XCTUnwrap(try sentEvents().first { $0.type == .error })
        XCTAssertEqual(crash.sessionId, "old-session")
        XCTAssertEqual(crash.viewName, "Cart")
        XCTAssertEqual(crash.error?.isCrash, true)
        XCTAssertEqual(crash.error?.source, "crash")
        XCTAssertEqual(crash.tsMs, 1_699_000_000_000)
    }

    func testBeforeSendCanDropAndMask() throws {
        StubProtocol.reset()
        var config = TrooperConfig(dsn: "https://k@ingest.test/1", app: "a", version: "1")
        config.trackCrashes = false
        config.beforeSend = { e in
            if e.custom?.name == "secret" { return nil }
            var m = e
            m.custom?.props = nil
            return m
        }
        var env = TrooperCore.Environment()
        env.defaults = freshDefaults()
        env.storageDirectory = tempDirectory()
        env.urlSession = StubProtocol.session()
        env.startTimer = false
        let c = TrooperCore(config: config, environment: env)!
        c.submit(.custom) { $0.custom = RumCustomPayload(name: "secret") }
        c.submit(.custom) { $0.custom = RumCustomPayload(name: "ok", props: ["email": "a@b.c"]) }
        flush(c)
        let customs = try sentEvents().filter { $0.type == .custom }
        XCTAssertEqual(customs.map { $0.custom?.name }, ["ok"])
        XCTAssertNil(customs.first?.custom?.props)
    }

    func testHybridContext() {
        let c = makeCore()
        c.prime()
        let ctx = c.nativeContext()
        XCTAssertNotNil(ctx?.sessionId)
        XCTAssertEqual(ctx?.anonymousId, c.anonymousId)
        let roundTrip = TrooperHybridContext(dictionary: ctx!.dictionary)
        XCTAssertEqual(roundTrip, ctx)
    }
}
