import Foundation
#if SWIFT_PACKAGE
@_spi(TrooperReplay) import TaskTrooper
#endif

/// Mobile session replay options (contracts.md §23, ADR-021). Wireframes only: no screenshots or pixels.
public struct TrooperReplayOptions: Sendable, Equatable {
    /// Fraction of (RUM-sampled) sessions recorded from the start. Decided once per session.
    public var sampleRate: Double
    /// Fraction of the remaining sessions that keep the last 60 s in memory and start uploading (and keep
    /// recording) when an ERROR (handled error, hang, network error) is recorded.
    public var onErrorSampleRate: Double
    /// Mask all text (length kept as `•`). Text fields and secure entry are always masked; opt views in with
    /// `view.trooperUnmask = true` / SwiftUI `.trooperUnmask()`.
    public var maskAllText: Bool
    /// `false` turns replay off entirely.
    public var enabled: Bool
    /// Offline cap for unsent segments; newer segments are dropped beyond it.
    public var maxQueueBytes: Int

    public init(sampleRate: Double = 0.1, onErrorSampleRate: Double = 1.0, maskAllText: Bool = true,
                enabled: Bool = true, maxQueueBytes: Int = 2 * 1024 * 1024) {
        self.sampleRate = sampleRate
        self.onErrorSampleRate = onErrorSampleRate
        self.maskAllText = maskAllText
        self.enabled = enabled
        self.maxQueueBytes = maxQueueBytes
    }
}

public extension TaskTrooper {
    /// Starts the SDK with wireframe session replay:
    ///
    /// ```swift
    /// TaskTrooper.start(config, replay: .init(sampleRate: 0.1, onErrorSampleRate: 1.0))
    /// ```
    static func start(_ config: TrooperConfig, replay: TrooperReplayOptions) {
        start(config)
        TrooperReplay.install(replay)
    }
}

/// Session replay entry points. Segments share the RUM `session_id` and go to `POST /v1/replay`.
/// iOS only (no-op on tvOS / macOS and in app extensions). Never throws.
public enum TrooperReplay {
    static let controller = ReplayLock<ReplayController?>(nil)

    /// Installs replay (after `TaskTrooper.start`; `TaskTrooper.start(_:replay:)` does both). Later calls are ignored.
    public static func install(_ options: TrooperReplayOptions = TrooperReplayOptions()) {
        guard options.enabled else { return }
        #if canImport(UIKit) && os(iOS)
        guard !ReplayController.isAppExtension else { return }
        let created = controller.withLock { (c: inout ReplayController?) -> ReplayController? in
            guard c == nil, let made = ReplayController(options: options) else { return nil }
            c = made
            return made
        }
        guard let created else { return }
        created.start()
        if Thread.isMainThread {
            MainActor.assumeIsolated { ReplayRecorder.install(controller: created, options: options) }
        } else {
            DispatchQueue.main.async { ReplayRecorder.install(controller: created, options: options) }
        }
        #endif
    }

    /// True while the current session is recorded (not just buffered for errors).
    public static var isRecording: Bool { controller.current?.isRecording.current ?? false }

    /// Seals the current segment and uploads pending segments; `completion` runs on a background queue.
    public static func flush(completion: (@Sendable () -> Void)? = nil) {
        guard let c = controller.current else { completion?(); return }
        c.flush(completion: completion)
    }
}

/// Owns the engine, store and uploader; everything runs on `queue`.
final class ReplayController: @unchecked Sendable {
    static let sdkVersion = "0.1.0"
    static let isAppExtension = Bundle.main.bundlePath.hasSuffix(".appex")

    let queue = DispatchQueue(label: "dev.tasktrooper.replay", qos: .utility)
    let engine: ReplayEngine
    let store: SegmentStore
    let uploader: SegmentUploader
    /// Mirrors of engine state for the main thread.
    let isCapturing = ReplayLock(false)
    let isRecording = ReplayLock(false)
    private var timer: DispatchSourceTimer?
    private var observing = false

    init?(options: TrooperReplayOptions, defaults: UserDefaults = .standard, directory: URL? = SegmentStore.defaultDirectory(),
          session: URLSession = URLSession(configuration: .ephemeral)) {
        guard let dir = directory, let store = SegmentStore(directory: dir, maxBytes: options.maxQueueBytes) else { return nil }
        self.store = store
        let uploader = SegmentUploader(queue: queue, store: store, session: session)
        self.uploader = uploader
        self.engine = ReplayEngine(
            options: options,
            now: { Int64((Date().timeIntervalSince1970 * 1000).rounded()) },
            store: DefaultsReplayStore(defaults),
            sink: { gz in
                if store.write(gz, nowMs: Int64(Date().timeIntervalSince1970 * 1000)) { uploader.drain() }
            },
            context: { TrooperReplayBridge.context() },
            resource: {
                TrooperReplayBridge.resource.map {
                    ReplayEngine.ResourceInfo(service: $0.service, environment: $0.environment, version: $0.version,
                                              sdkVersion: ReplayController.sdkVersion)
                }
            })
    }

    func start() {
        queue.async {
            let t = DispatchSource.makeTimerSource(queue: self.queue)
            t.schedule(deadline: .now() + 1, repeating: 1, leeway: .milliseconds(250))
            t.setEventHandler { self.tick() } // the controller lives for the process
            t.resume()
            self.timer = t
            self.engine.refreshSession()
            self.publish()
            self.uploader.drain() // segments left by previous launches (offline)
        }
    }

    private func tick() {
        if !observing {
            observing = TrooperReplayBridge.addErrorObserver { [weak self] ts in
                guard let self else { return }
                self.queue.async { self.engine.onError(ts: ts); self.publish() }
            }
        }
        engine.tick()
    }

    /// Main-thread state mirrors; call after every engine mutation.
    func publish() {
        isCapturing.set(engine.isCapturing)
        isRecording.set(engine.mode == .record)
    }

    func refreshSession(_ done: @escaping @Sendable (Bool) -> Void) {
        queue.async {
            self.engine.refreshSession()
            self.publish()
            done(self.engine.isCapturing)
        }
    }

    func frame(ts: Int64, width: Int, height: Int, density: Double, nodes: [WireNode]) {
        queue.async {
            self.engine.onFrame(ts: ts, width: width, height: height, density: density, nodes: nodes)
            self.publish()
        }
    }

    func touch(ts: Int64, x: Double, y: Double, phase: String) {
        queue.async { self.engine.onTouch(ts: ts, x: x, y: y, phase: phase) }
    }

    func keyboard(ts: Int64, visible: Bool) {
        queue.async { self.engine.onKeyboard(ts: ts, visible: visible) }
    }

    func flush(completion: (@Sendable () -> Void)? = nil) {
        queue.async {
            self.engine.flush()
            self.uploader.drain { completion?() }
        }
    }
}

/// Lock-protected box (the core's `Locked` is internal to the TaskTrooper module).
final class ReplayLock<Value>: @unchecked Sendable {
    private let lock = NSLock()
    private var value: Value
    init(_ value: Value) { self.value = value }

    @discardableResult
    func withLock<R>(_ body: (inout Value) throws -> R) rethrows -> R {
        lock.lock()
        defer { lock.unlock() }
        return try body(&value)
    }

    var current: Value { withLock { $0 } }
    func set(_ v: Value) { withLock { $0 = v } }
}
