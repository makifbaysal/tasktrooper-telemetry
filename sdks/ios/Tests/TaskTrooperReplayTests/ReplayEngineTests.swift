import XCTest
@_spi(TrooperReplay) import TaskTrooper
@testable import TaskTrooperReplay

final class MemoryReplayStore: ReplayKeyValueStore {
    var values: [String: String] = [:]
    func string(forKey key: String) -> String? { values[key] }
    func set(_ v: [String: String]) { values.merge(v) { _, new in new } }
}

final class EngineHarness {
    var now: Int64 = 1_700_000_000_000
    var ctx: TrooperReplayBridge.Context? = .init(sessionId: "s1", sampled: true, anonymousId: "anon-1", userId: "user-7",
                                                  viewId: "v1", viewName: "Checkout")
    var segments: [[String: Any]] = []
    let store: MemoryReplayStore
    var engine: ReplayEngine!

    init(_ options: TrooperReplayOptions = .init(sampleRate: 1), random: [Double] = [0.5], store: MemoryReplayStore = MemoryReplayStore()) {
        self.store = store
        var randoms = random
        engine = ReplayEngine(
            options: options, now: { self.now }, store: store,
            sink: { gz in
                let json = TrooperReplayBridge.gunzip(gz)!
                self.segments.append(try! JSONSerialization.jsonObject(with: json) as! [String: Any])
            },
            context: { self.ctx },
            resource: { .init(service: "shop-ios", environment: "production", version: "1.4.0", sdkVersion: "0.1.0") },
            random: { randoms.count > 1 ? randoms.removeFirst() : randoms[0] })
    }

    func frame(_ nodes: WireNode...) {
        engine.onFrame(ts: now, width: 390, height: 844, density: 3, nodes: nodes)
    }

    func advance(_ ms: Int64) { now += ms; engine.tick() }

    static func types(_ seg: [String: Any]) -> [String] {
        (seg["events"] as! [[String: Any]]).map { $0["type"] as! String }
    }
}

func node(_ id: Int, _ text: String? = nil) -> WireNode {
    WireNode(id: id, x: 0, y: 0, w: 100, h: 40, kind: "text", text: text)
}

final class ReplayEngineTests: XCTestCase {
    func testSegmentPayloadMatchesContract() throws {
        let h = EngineHarness()
        h.frame(node(1, "••••"))
        h.engine.onKeyboard(ts: h.now + 50, visible: true)
        h.engine.onTouch(ts: h.now + 100, x: 10, y: 20, phase: "down")
        h.engine.onTouch(ts: h.now + 150, x: 10, y: 20, phase: "up")
        h.engine.flush()

        XCTAssertEqual(h.segments.count, 1)
        let s = h.segments[0]
        let res = try XCTUnwrap(s["resource"] as? [String: Any])
        XCTAssertEqual(res["service"] as? String, "shop-ios")
        XCTAssertEqual(res["env"] as? String, "production")
        XCTAssertEqual(res["version"] as? String, "1.4.0")
        XCTAssertEqual(res["platform"] as? String, "ios")
        XCTAssertEqual(res["sdk_name"] as? String, ReplayEngine.sdkName)
        XCTAssertEqual(s["session_id"] as? String, "s1")
        XCTAssertEqual(s["segment"] as? Int, 0)
        XCTAssertEqual(s["format"] as? String, "wireframe")
        XCTAssertEqual(s["has_full_snapshot"] as? Bool, true)
        XCTAssertEqual((s["start_ts"] as? NSNumber)?.int64Value, h.now)
        XCTAssertEqual((s["end_ts"] as? NSNumber)?.int64Value, h.now + 150)
        XCTAssertEqual(s["view_id"] as? String, "v1")
        XCTAssertEqual(s["view_name"] as? String, "Checkout")
        XCTAssertEqual(s["user_id"] as? String, "user-7")
        XCTAssertEqual(s["anonymous_id"] as? String, "anon-1")
        XCTAssertEqual((s["stats"] as? [String: Any])?["clicks"] as? Int, 1)
        XCTAssertEqual(EngineHarness.types(s), ["screen", "frame", "keyboard", "touch", "touch"])

        let events = s["events"] as! [[String: Any]]
        XCTAssertEqual(events[0]["width"] as? Int, 390)
        XCTAssertEqual(events[0]["density"] as? Int, 3)
        XCTAssertEqual(events[0]["name"] as? String, "Checkout")
        let n = (events[1]["nodes"] as! [[String: Any]])[0]
        XCTAssertEqual(n["id"] as? Int, 1)
        XCTAssertEqual(n["kind"] as? String, "text")
        XCTAssertEqual(events[2]["visible"] as? Bool, true)
        XCTAssertEqual(events[3]["phase"] as? String, "down")
    }

    func testUnchangedFramesAreDropped() {
        let h = EngineHarness()
        h.frame(node(1, "a")); h.now += 500
        h.frame(node(1, "a")); h.now += 500
        h.frame(node(1, "b"))
        h.engine.flush()
        XCTAssertEqual(EngineHarness.types(h.segments[0]), ["screen", "frame", "frame"])
    }

    func testSegmentsSealAfterFiveSecondsAndStartWithScreen() {
        let h = EngineHarness()
        h.frame(node(1, "a"))
        h.advance(4_000)
        XCTAssertTrue(h.segments.isEmpty)
        h.engine.onTouch(ts: h.now, x: 1, y: 1, phase: "down")
        h.advance(1_000)
        XCTAssertEqual(h.segments.count, 1)
        h.frame(node(1, "b"))
        h.engine.flush()
        XCTAssertEqual(h.segments.map { $0["segment"] as? Int }, [0, 1])
        XCTAssertEqual(EngineHarness.types(h.segments[1]), ["screen", "frame"])
        XCTAssertEqual(h.store.values[ReplayEngine.keySegment], "2")
    }

    func testSegmentsSealAtSizeLimit() {
        let h = EngineHarness()
        let big = String(repeating: "x", count: 200)
        var id = 0
        while h.segments.isEmpty, id < 5_000 {
            h.engine.onFrame(ts: h.now, width: 1, height: 1, density: 1, nodes: (0..<10).map { _ in id += 1; return node(id, big) })
        }
        XCTAssertEqual(h.segments.count, 1)
    }

    func testSegmentCounterPerSession() {
        let store = MemoryReplayStore()
        let h1 = EngineHarness(store: store)
        h1.frame(node(1)); h1.engine.flush(); h1.frame(node(2)); h1.engine.flush()
        XCTAssertEqual(h1.segments.map { $0["segment"] as? Int }, [0, 1])

        let h2 = EngineHarness(store: store) // relaunch, same RUM session
        h2.frame(node(3)); h2.engine.flush()
        XCTAssertEqual(h2.segments[0]["segment"] as? Int, 2)

        h2.frame(node(4))
        h2.ctx = .init(sessionId: "s2", sampled: true, anonymousId: "anon-1", viewName: "Home")
        h2.frame(node(5))
        h2.engine.flush()
        XCTAssertEqual(h2.segments.map { "\($0["session_id"]!)/\($0["segment"]!)" }, ["s1/2", "s1/3", "s2/0"])
    }

    func testSamplingDecision() {
        XCTAssertEqual(EngineHarness(.init(), random: [0.05]).engine.refreshSession(), .record)
        XCTAssertEqual(EngineHarness(.init(), random: [0.5, 0.5]).engine.refreshSession(), .buffer)
        XCTAssertEqual(EngineHarness(.init(onErrorSampleRate: 0), random: [0.5]).engine.refreshSession(), .off)
        let unsampled = EngineHarness(.init(sampleRate: 1))
        unsampled.ctx?.sampled = false
        XCTAssertEqual(unsampled.engine.refreshSession(), .off)
        XCTAssertEqual(EngineHarness(.init(sampleRate: 1, enabled: false)).engine.refreshSession(), .off)
        let noSdk = EngineHarness()
        noSdk.ctx = nil
        XCTAssertEqual(noSdk.engine.refreshSession(), .off)
    }

    func testOnErrorBufferKeepsLastMinuteAndFlushesOnError() {
        let h = EngineHarness(.init(sampleRate: 0, onErrorSampleRate: 1))
        h.frame(node(1, "first"))
        h.advance(30_000)
        h.frame(node(2, "second"))
        h.advance(45_000)
        h.engine.onTouch(ts: h.now, x: 5, y: 5, phase: "down")
        h.advance(20_000) // "second" (65 s old) is now the state at the window start; "first" is dropped
        h.engine.flush()
        XCTAssertTrue(h.segments.isEmpty, "buffered data is not sent without an error")

        h.engine.onError(ts: h.now)
        XCTAssertEqual(h.engine.mode, .record)
        XCTAssertEqual(h.segments.count, 1)
        let s = h.segments[0]
        XCTAssertEqual(EngineHarness.types(s), ["screen", "frame", "touch"])
        let firstFrame = (s["events"] as! [[String: Any]])[1]["nodes"] as! [[String: Any]]
        XCTAssertEqual(firstFrame[0]["id"] as? Int, 2)
        XCTAssertEqual((s["stats"] as? [String: Any])?["errors"] as? Int, 1)

        h.frame(node(3)); h.engine.flush()
        XCTAssertEqual(h.segments.count, 2)
        XCTAssertEqual(h.segments[1]["segment"] as? Int, 1)
    }

    func testOffModeRecordsNothing() {
        let h = EngineHarness(.init(sampleRate: 0, onErrorSampleRate: 0))
        h.frame(node(1)); h.engine.onTouch(ts: h.now, x: 1, y: 1, phase: "down"); h.engine.onError(ts: h.now); h.engine.flush()
        XCTAssertFalse(h.engine.isCapturing)
        XCTAssertTrue(h.segments.isEmpty)
    }
}
