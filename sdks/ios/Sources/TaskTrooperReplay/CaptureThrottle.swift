import Foundation

/// Capture scheduling of the replay recorder (platform independent so it can be unit tested).
///
/// The main run loop going idle after doing work means the UI may have changed. A capture runs at most every
/// `interval` (≥ 0.5 s = 2 fps; raised to ≈ 100 × the last capture cost, ≤ 5 s) with one trailing capture so the
/// final state after a burst is recorded. The trailing capture is skipped when another capture already ran after
/// it was scheduled (timer leeway often lets a run loop wake-up reach the due time first: without the check
/// every period captured twice).
struct CaptureThrottle {
    static let minInterval: TimeInterval = 0.5
    static let maxInterval: TimeInterval = 5
    static let truncatedInterval: TimeInterval = 2

    enum Action: Equatable {
        case none
        case capture
        /// Schedule `trailingFired()` after the delay (seconds).
        case scheduleTrailing(TimeInterval)
    }

    private(set) var interval = CaptureThrottle.minInterval
    private(set) var lastCapture: TimeInterval = -.infinity
    private var trailingScheduledAt: TimeInterval?
    /// The next idle notification comes from our own (trailing) capture's run loop iteration, not from a UI change.
    private var ownWake = false

    /// Main run loop is about to sleep.
    mutating func runLoopIdle(now: TimeInterval) -> Action {
        if ownWake { ownWake = false; return .none }
        let due = lastCapture + interval
        if now >= due { return .capture }
        if trailingScheduledAt != nil { return .none }
        trailingScheduledAt = now
        return .scheduleTrailing(due - now)
    }

    /// The trailing timer fired; true = capture now.
    mutating func trailingFired() -> Bool {
        guard let scheduledAt = trailingScheduledAt else { return false }
        trailingScheduledAt = nil
        if lastCapture >= scheduledAt { return false } // a capture after scheduling already saw the final state
        ownWake = true
        return true
    }

    /// A capture is about to run outside the run loop observer (e.g. right after the session started).
    mutating func markOwnWake() { ownWake = true }

    /// Records a capture that ended at `now` and took `cost` seconds of main thread.
    mutating func didCapture(now: TimeInterval, cost: TimeInterval, truncated: Bool) {
        lastCapture = now
        // Budget ≈ 1% of main-thread time: interval = cost × 100, clamped to [0.5 s, 5 s].
        interval = min(max(cost * 100, Self.minInterval), Self.maxInterval)
        if truncated { interval = max(interval, Self.truncatedInterval) }
    }
}
