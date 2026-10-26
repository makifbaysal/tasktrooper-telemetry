import Foundation

/// Injectable time source. `nowMs` is wall-clock (event timestamps), `uptime` is monotonic seconds (durations).
struct TrooperClock: Sendable {
    var nowMs: @Sendable () -> Int64
    var uptime: @Sendable () -> TimeInterval

    static let system = TrooperClock(
        nowMs: { Int64((Date().timeIntervalSince1970 * 1000).rounded()) },
        uptime: { ProcessInfo.processInfo.systemUptime }
    )
}
