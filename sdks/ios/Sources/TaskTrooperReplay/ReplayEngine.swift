import Foundation
#if SWIFT_PACKAGE
@_spi(TrooperReplay) import TaskTrooper
#endif

/// Persistent key/values for the replay engine (UserDefaults on device).
protocol ReplayKeyValueStore: AnyObject {
    func string(forKey key: String) -> String?
    func set(_ values: [String: String])
}

final class DefaultsReplayStore: ReplayKeyValueStore {
    private let defaults: UserDefaults
    private let prefix = "dev.tasktrooper.replay."
    init(_ defaults: UserDefaults = .standard) { self.defaults = defaults }
    func string(forKey key: String) -> String? { defaults.string(forKey: prefix + key) }
    func set(_ values: [String: String]) { for (k, v) in values { defaults.set(v, forKey: prefix + k) } }
}

/// Sampling, on-error ring buffer and segmenting for wireframe replay (contracts.md §23). Platform
/// independent. Not thread-safe: the owner confines it to one serial queue.
///
/// - Per RUM session: `sampleRate` → `.record`; else `onErrorSampleRate` → `.buffer` (last 60 s in memory;
///   on ERROR the buffer is sent and recording continues); else `.off`. Fixed for the session.
/// - A segment is sealed when ≥ 5 s old or ≥ 256 KB (uncompressed), on `flush()` (app in background) and on
///   session change. Every segment starts with a `screen` event.
final class ReplayEngine {
    enum Mode: String { case off, buffer, record }

    struct ResourceInfo: Equatable {
        var service: String
        var environment: String
        var version: String
        var sdkVersion: String
    }

    typealias Context = TrooperReplayBridge.Context

    static let segmentMaxMs: Int64 = 5_000
    static let segmentMaxBytes = 256 * 1024
    static let bufferMs: Int64 = 60_000
    static let bufferMaxBytes = 4 * 1024 * 1024
    static let sdkName = "TaskTrooperReplay-iOS"
    static let keySession = "session_id", keySegment = "next_segment", keyMode = "mode"

    fileprivate enum EventType { case screen, frame, touch, keyboard }
    fileprivate struct Event { var ts: Int64; var json: String; var type: EventType; var bytes: Int }
    private struct Screen: Equatable { var width: Int; var height: Int; var density: Double; var name: String? }

    private let options: TrooperReplayOptions
    private let store: ReplayKeyValueStore
    private let sink: (Data) -> Void
    private let context: () -> Context?
    private let resource: () -> ResourceInfo?
    private let random: () -> Double
    private let now: () -> Int64

    private(set) var mode: Mode = .off
    private(set) var sessionId: String?
    private(set) var segment = 0
    private var events: [Event] = []
    private var bytes = 0
    private var segmentContext: Context?
    private var lastContext: Context?
    private var clicks = 0
    private var errors = 0
    private var lastScreen: Screen?
    private var lastFrameKey: String?
    private var lastKeyboard: Bool?

    var isCapturing: Bool { mode != .off }

    init(options: TrooperReplayOptions, now: @escaping () -> Int64, store: ReplayKeyValueStore,
         sink: @escaping (Data) -> Void, context: @escaping () -> Context?,
         resource: @escaping () -> ResourceInfo?, random: @escaping () -> Double = { Double.random(in: 0..<1) }) {
        self.options = options
        self.now = now
        self.store = store
        self.sink = sink
        self.context = context
        self.resource = resource
        self.random = random
    }

    // MARK: - Session

    /// Re-reads the RUM context; a new session id gets a new sampling decision.
    @discardableResult
    func refreshSession() -> Mode {
        guard let ctx = context() else {
            if sessionId == nil { mode = .off }
            return mode
        }
        lastContext = ctx
        if ctx.sessionId != sessionId { startSession(ctx) }
        return mode
    }

    private func startSession(_ ctx: Context) {
        if mode == .record { seal() }
        resetBuffer()
        lastScreen = nil; lastFrameKey = nil; lastKeyboard = nil
        sessionId = ctx.sessionId
        if store.string(forKey: Self.keySession) == ctx.sessionId {
            // Same RUM session after a relaunch: continue numbering, keep the decision.
            segment = Int(store.string(forKey: Self.keySegment) ?? "") ?? 0
            mode = Mode(rawValue: store.string(forKey: Self.keyMode) ?? "") ?? decide(ctx)
        } else {
            segment = 0
            mode = decide(ctx)
        }
        persistState()
    }

    private func decide(_ ctx: Context) -> Mode {
        guard options.enabled, ctx.sampled else { return .off }
        if hit(options.sampleRate) { return .record }
        if hit(options.onErrorSampleRate) { return .buffer }
        return .off
    }

    private func hit(_ rate: Double) -> Bool { rate >= 1 || (rate > 0 && random() < rate) }

    private func persistState() {
        guard let sid = sessionId else { return }
        store.set([Self.keySession: sid, Self.keySegment: String(segment), Self.keyMode: mode.rawValue])
    }

    // MARK: - Input

    /// A full wireframe; recorded only when it differs from the previous frame.
    func onFrame(ts: Int64, width: Int, height: Int, density: Double, nodes: [WireNode]) {
        guard refreshSession() != .off else { return }
        let screen = Screen(width: width, height: height, density: density, name: lastContext?.viewName)
        if screen != lastScreen {
            lastScreen = screen
            lastFrameKey = nil
            add(Event(ts: ts, json: WireEvents.screen(ts: ts, width: width, height: height, density: density, name: screen.name), type: .screen))
        }
        let key = WireEvents.nodesKey(nodes)
        if key != lastFrameKey {
            lastFrameKey = key
            add(Event(ts: ts, json: WireEvents.frame(ts: ts, nodesKey: key), type: .frame))
        }
        maybeSeal(now: ts)
    }

    func onTouch(ts: Int64, x: Double, y: Double, phase: String) {
        guard mode != .off else { return }
        if phase == "down" { clicks += 1 }
        add(Event(ts: ts, json: WireEvents.touch(ts: ts, x: x, y: y, phase: phase), type: .touch))
        maybeSeal(now: ts)
    }

    func onKeyboard(ts: Int64, visible: Bool) {
        guard mode != .off, visible != lastKeyboard else { return }
        lastKeyboard = visible
        add(Event(ts: ts, json: WireEvents.keyboard(ts: ts, visible: visible), type: .keyboard))
    }

    /// RUM ERROR of the current session: flush the on-error buffer and keep recording.
    func onError(ts: Int64) {
        switch mode {
        case .off: return
        case .buffer:
            errors += 1
            mode = .record
            persistState()
            sealChunks()
        case .record:
            errors += 1
        }
    }

    /// Timer (≈ 1 s): seals due segments, trims the ring buffer.
    func tick() {
        switch mode {
        case .record: maybeSeal(now: now())
        case .buffer: trim(now: now())
        case .off: break
        }
    }

    /// App went to background: send what is recorded (buffered on-error data stays in memory).
    func flush() {
        if mode == .record { seal() }
    }

    // MARK: - Buffer & segments

    private func add(_ e: Event) {
        var e = e
        e.bytes = e.json.utf8.count
        if events.isEmpty {
            segmentContext = lastContext
            if e.type != .screen, mode == .record, let s = lastScreen {
                let json = WireEvents.screen(ts: e.ts, width: s.width, height: s.height, density: s.density, name: s.name)
                events.append(Event(ts: e.ts, json: json, type: .screen, bytes: json.utf8.count))
                bytes += json.utf8.count
            }
        }
        events.append(e)
        bytes += e.bytes
        if mode == .buffer { trim(now: e.ts) }
    }

    private func maybeSeal(now: Int64) {
        guard mode == .record, let first = events.first else { return }
        if now - first.ts >= Self.segmentMaxMs || bytes >= Self.segmentMaxBytes { seal() }
    }

    /// Keeps the last 60 s plus the latest full frame (and screen) before that, capped at 4 MB.
    private func trim(now: Int64) {
        let cutoff = now - Self.bufferMs
        var lastOldFrame = -1
        for (i, e) in events.enumerated() {
            if e.ts > cutoff { break }
            if e.type == .frame { lastOldFrame = i }
        }
        if lastOldFrame > 0 {
            let head = events[0..<lastOldFrame]
            let screen = head.last { $0.type == .screen }
            bytes -= head.reduce(0) { $0 + $1.bytes }
            events.removeFirst(lastOldFrame)
            if let screen { events.insert(screen, at: 0); bytes += screen.bytes }
        }
        while bytes > Self.bufferMaxBytes, events.count > 1 { bytes -= events.removeFirst().bytes }
    }

    private func sealChunks() {
        while !events.isEmpty {
            if bytes <= Self.segmentMaxBytes { return seal() }
            var n = 0, size = 0
            while n < events.count, n == 0 || size + events[n].bytes <= Self.segmentMaxBytes { size += events[n].bytes; n += 1 }
            let chunk = Array(events[0..<n])
            events.removeFirst(n)
            bytes -= size
            write(chunk, clicks: clicks, errors: errors)
            clicks = 0; errors = 0
            segmentContext = lastContext
        }
    }

    private func seal() {
        guard !events.isEmpty else { return }
        let chunk = events
        let c = clicks, e = errors
        resetBuffer()
        write(chunk, clicks: c, errors: e)
    }

    private func resetBuffer() {
        events.removeAll(keepingCapacity: true)
        bytes = 0; clicks = 0; errors = 0
    }

    private func write(_ chunk: [Event], clicks: Int, errors: Int) {
        guard let sid = sessionId, let res = resource(), let first = chunk.first else { return }
        let ctx = segmentContext ?? lastContext
        var j = JSONText(capacity: 512 + chunk.reduce(0) { $0 + $1.bytes + 1 })
        j.beginObject()
        j.key("resource"); j.beginObject()
        j.key("service"); j.string(res.service)
        j.key("env"); j.string(res.environment)
        j.key("version"); j.string(res.version)
        j.key("platform"); j.string("ios")
        j.key("sdk_name"); j.string(Self.sdkName)
        j.key("sdk_version"); j.string(res.sdkVersion)
        j.endObject()
        j.key("session_id"); j.string(sid)
        j.key("segment"); j.int(segment)
        j.key("start_ts"); j.int64(first.ts)
        j.key("end_ts"); j.int64(chunk.map(\.ts).max() ?? first.ts)
        j.key("format"); j.string("wireframe")
        j.key("has_full_snapshot"); j.bool(chunk.contains { $0.type == .frame })
        if let ctx {
            if let v = ctx.viewId { j.key("view_id"); j.string(v) }
            if let v = ctx.viewName { j.key("view_name"); j.string(v) }
            if let v = ctx.userId { j.key("user_id"); j.string(v) }
            if !ctx.anonymousId.isEmpty { j.key("anonymous_id"); j.string(ctx.anonymousId) }
        }
        // Rage clicks / inputs are not detected on mobile wireframes (optional, omitted).
        j.key("stats"); j.beginObject()
        j.key("clicks"); j.int(clicks)
        j.key("errors"); j.int(errors)
        j.endObject()
        j.key("events"); j.beginArray()
        for e in chunk { j.raw(e.json) }
        j.endArray()
        j.endObject()
        guard let gz = TrooperReplayBridge.gzip(Data(j.text.utf8)) else { return }
        sink(gz)
        segment += 1
        persistState()
    }
}

private extension ReplayEngine.Event {
    init(ts: Int64, json: String, type: ReplayEngine.EventType) { self.init(ts: ts, json: json, type: type, bytes: 0) }
}
