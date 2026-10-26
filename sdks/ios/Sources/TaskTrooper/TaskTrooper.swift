import Foundation

/// TaskTrooper RUM for iOS / tvOS.
///
/// ```swift
/// TaskTrooper.start(TrooperConfig(dsn: "https://<key>@ingest.example.com/1",
///                                 app: "shop-ios", version: "1.4.0", environment: "production",
///                                 tracePropagationTargets: ["api.shop.com"]))
/// ```
/// All methods are thread-safe and never throw; SDK failures are swallowed (logged with `debug = true`).
public enum TaskTrooper {
    static let core = Locked<TrooperCore?>(nil)
    static let hangDetector = Locked<HangDetector?>(nil)
    private static let reachability = Locked<Reachability?>(nil)

    public static var isStarted: Bool { core.current != nil }

    /// Starts the SDK. Call once, as early as possible (`application(_:didFinishLaunchingWithOptions:)`
    /// or your SwiftUI `App.init`). Subsequent calls are ignored.
    public static func start(_ config: TrooperConfig) {
        _ = start(config, environment: TrooperCore.Environment())
    }

    @discardableResult
    static func start(_ config: TrooperConfig, environment: TrooperCore.Environment, installUI: Bool = true) -> TrooperCore? {
        TrooperLog.enabled.set(config.debug)
        if let existing = core.current {
            TrooperLog.debug("start() called twice; ignored")
            return existing
        }
        guard let c = TrooperCore(config: config, environment: environment) else {
            TrooperLog.debug("invalid DSN or storage; SDK disabled")
            return nil
        }
        core.set(c)

        if config.trackCrashes {
            let url = CrashReporter.reportURL(in: c.storageDirectory)
            if let report = CrashReporter.loadPending(at: url) { c.emitCrash(report) }
            CrashReporter.install(at: url, signals: true, exceptions: true)
        }
        c.prime()

        let r = Reachability()
        r.start { conn in
            c.connection.set(conn)
            if conn != "offline" { c.queue.async { c.uploader.drain() } }
        }
        reachability.set(r)

        if config.trackNetwork {
            NetworkInstrumentation.shared.enable(.init(
                propagation: TracePropagation(targets: config.tracePropagationTargets),
                shouldPropagate: { c.sessionSampled.current },
                onComplete: { c.recordResource($0) },
                clock: c.clock,
                serviceName: config.app))
        }

        if config.trackHangs {
            let threshold = max(0.5, config.hangThreshold)
            let h = HangDetector(threshold: threshold, clock: c.clock) { duration, frames in
                let ms = duration * 1000
                let stack = frames.map { HangDetector.stackText($0) }
                c.submit(.error, ts: c.clock.nowMs() - Int64(ms)) { e in
                    e.error = RumErrorPayload(
                        type: "AppHang", message: String(format: "Main thread unresponsive for %.0f ms", ms),
                        stack: stack, source: "anr", isCrash: false, thread: "main", fingerprint: nil)
                    e.attributes = ["hang.duration_ms": String(format: "%.0f", ms)]
                }
            }
            h.start()
            hangDetector.set(h)
        }

        #if canImport(UIKit)
        if installUI {
            if Thread.isMainThread {
                MainActor.assumeIsolated { UIKitIntegration.install(core: c, config: config) }
            } else {
                DispatchQueue.main.async { UIKitIntegration.install(core: c, config: config) }
            }
        }
        #endif
        return c
    }

    // MARK: - Users

    /// Associates subsequent events with a user. Only what you pass is sent; PII is your responsibility.
    public static func identify(id: String, traits: [String: String] = [:]) {
        core.current?.identify(RumUser(id: id, traits: traits.isEmpty ? nil : traits))
    }

    public static func clearUser() {
        core.current?.identify(nil)
    }

    // MARK: - Events

    /// Custom event (`CUSTOM`).
    public static func track(_ name: String, properties: [String: String] = [:], value: Double? = nil) {
        core.current?.submit(.custom) { e in
            e.custom = RumCustomPayload(name: name, props: properties.isEmpty ? nil : properties, value: value)
        }
    }

    /// User action (`ACTION`), e.g. a tap on a button.
    public static func trackAction(_ name: String, target: String? = nil) {
        core.current?.submit(.action) { e in
            e.action = RumActionPayload(name: name, target: target, rage: nil, dead: nil)
        }
    }

    /// Handled error (`ERROR`, source "source").
    public static func captureError(_ error: Error, attributes: [String: String] = [:]) {
        let ns = error as NSError
        let type = "\(Swift.type(of: error))"
        let message = (error as? LocalizedError)?.errorDescription ?? "\(ns.domain) (\(ns.code)): \(ns.localizedDescription)"
        captureError(message: message, type: type, attributes: attributes)
    }

    public static func captureError(message: String, type: String = "Error", attributes: [String: String] = [:]) {
        guard let c = core.current else { return }
        let stack = Thread.callStackSymbols.dropFirst(2).joined(separator: "\n")
        let thread = Thread.isMainThread ? "main" : (Thread.current.name.flatMap { $0.isEmpty ? nil : $0 } ?? "background")
        c.submit(.error) { e in
            e.error = RumErrorPayload(type: type, message: message, stack: stack, source: "source",
                                      isCrash: false, thread: thread, fingerprint: nil)
            e.attributes = attributes.isEmpty ? nil : attributes
        }
    }

    // MARK: - Views

    /// Starts a view (screen). The previous view ends. `key` identifies the instance (defaults to `name`).
    public static func startView(_ name: String, key: String? = nil, attributes: [String: String] = [:]) {
        core.current?.startView(name: name, key: key ?? name, attributes: attributes)
    }

    /// Ends the view started with `key` (or with this name when no key was given).
    public static func stopView(_ key: String) {
        core.current?.stopView(key: key)
    }

    static func startView(_ name: String, key: String, loadingTimeMs: Double?) {
        core.current?.startView(name: name, key: key, loadingTimeMs: loadingTimeMs)
    }

    // MARK: - Delivery

    /// Sends buffered events now. `completion` runs on a background queue once the queue is drained
    /// (or a retryable failure stops it).
    public static func flush(completion: (@Sendable () -> Void)? = nil) {
        guard let c = core.current else {
            completion?()
            return
        }
        c.flush(completion: completion)
    }

    /// Testing only: forget the current core (swizzles stay installed but become no-ops).
    static func resetForTesting() {
        NetworkInstrumentation.shared.disable()
        hangDetector.current?.pause()
        hangDetector.set(nil)
        core.set(nil)
    }
}
