import Foundation
#if canImport(TaskTrooperCrashC)
import TaskTrooperCrashC
#endif

/// Pure watchdog state: one outstanding ping to the main thread at a time; when it is answered, the
/// time it waited is the main-thread block duration.
struct HangState {
    let threshold: TimeInterval
    private(set) var pendingSince: TimeInterval?
    private var captured = false

    init(threshold: TimeInterval) { self.threshold = threshold }

    /// Returns true when a new ping should be posted.
    mutating func shouldPing(now: TimeInterval) -> Bool {
        guard pendingSince == nil else { return false }
        pendingSince = now
        return true
    }

    /// True once per pending ping when it has waited ≥ threshold: time to sample the main thread's stack
    /// (while it is still blocked).
    mutating func shouldCapture(now: TimeInterval) -> Bool {
        guard let since = pendingSince, !captured, now - since >= threshold else { return false }
        captured = true
        return true
    }

    /// Main thread answered. Returns the hang duration if it reached the threshold.
    mutating func pong(now: TimeInterval) -> TimeInterval? {
        guard let since = pendingSince else { return nil }
        pendingSince = nil
        captured = false
        let d = now - since
        return d >= threshold ? d : nil
    }

    mutating func reset() {
        pendingSince = nil
        captured = false
    }
}

/// Main-thread watchdog (iOS equivalent of Android ANR). Pings the main queue every 250 ms from a
/// background queue. When a ping has waited ≥ threshold, the main thread is briefly suspended and its
/// stack sampled (frame-pointer walk); the hang (with that stack) is reported once the main thread recovers.
final class HangDetector: @unchecked Sendable {
    private let state: Locked<HangState>
    private let paused = Locked(false)
    private let stack = Locked<[UInt64]?>(nil)
    private let clock: TrooperClock
    private let captureMainStack: @Sendable () -> [UInt64]
    private let onHang: @Sendable (TimeInterval, [UInt64]?) -> Void
    private let queue = DispatchQueue(label: "dev.tasktrooper.watchdog", qos: .utility)
    private var timer: DispatchSourceTimer?

    init(threshold: TimeInterval, clock: TrooperClock = .system,
         captureMainStack: @escaping @Sendable () -> [UInt64] = HangDetector.sampleMainThread,
         onHang: @escaping @Sendable (TimeInterval, [UInt64]?) -> Void) {
        state = Locked(HangState(threshold: threshold))
        self.clock = clock
        self.captureMainStack = captureMainStack
        self.onHang = onHang
    }

    /// Suspends the main thread, walks its frame-pointer chain and resumes it (~tens of µs).
    @Sendable static func sampleMainThread() -> [UInt64] {
        let port = tt_main_thread_port()
        var buf = [UInt](repeating: 0, count: 128)
        let n = buf.withUnsafeMutableBufferPointer { tt_capture_thread_stack(port, $0.baseAddress, Int32($0.count)) }
        return buf.prefix(Int(max(0, n))).map { UInt64($0) }
    }

    /// Apple-format stack ("Thread 0 name: main" + frames + Binary Images) for a hang report.
    static func stackText(_ frames: [UInt64]) -> String {
        AppleStackFormatter.format([.init(index: 0, crashed: false, name: "main", frames: frames)],
                                   images: AppleStackFormatter.loadedImages())
    }

    func start() {
        queue.async { [self] in
            guard timer == nil else { return }
            let t = DispatchSource.makeTimerSource(queue: queue)
            t.schedule(deadline: .now() + 0.25, repeating: 0.25, leeway: .milliseconds(50))
            t.setEventHandler { [weak self] in self?.tick() }
            t.resume()
            timer = t
        }
    }

    func pause() {
        paused.set(true)
        state.withLock { $0.reset() }
    }

    func resume() {
        state.withLock { $0.reset() }
        paused.set(false)
    }

    private func tick() {
        guard !paused.current else { return }
        let now = clock.uptime()
        if state.withLock({ $0.shouldCapture(now: now) }) {
            let frames = captureMainStack()
            stack.set(frames.isEmpty ? nil : frames)
        }
        guard state.withLock({ $0.shouldPing(now: now) }) else { return }
        stack.set(nil)
        DispatchQueue.main.async { [self] in
            let d = state.withLock { $0.pong(now: clock.uptime()) }
            let frames = stack.current
            stack.set(nil)
            guard let d, !paused.current, tt_debugger_attached() == 0 else { return }
            onHang(d, frames)
        }
    }
}
