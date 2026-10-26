import Foundation

/// Slow / frozen frame accounting (contracts.md §9: slow > 16 ms, frozen > 700 ms). A frame is
/// slow when it exceeds the 60 Hz budget (16.7 ms, +5 % vsync jitter tolerance) regardless of the
/// display refresh rate; frozen frames are not also counted as slow. The server computes the ratio.
struct FrameStats: Equatable, Sendable {
    static let slowThreshold: TimeInterval = (1.0 / 60) * 1.05
    static let frozenThreshold: TimeInterval = 0.7
    /// Gaps longer than this are app suspension / debugger pauses, not frames.
    static let maxPlausible: TimeInterval = 30

    var total = 0
    var slow = 0
    var frozen = 0

    mutating func record(duration: TimeInterval) {
        guard duration > 0, duration < Self.maxPlausible else { return }
        total += 1
        if duration > Self.frozenThreshold { frozen += 1 }
        else if duration > Self.slowThreshold { slow += 1 }
    }

    /// Per-view counts; `attributes.total_frames` is added by the caller.
    func vitals() -> [RumVitalPayload] {
        guard total > 0 else { return [] }
        return [
            RumVitalPayload(name: "SLOW_FRAMES", value: Double(slow), rating: nil),
            RumVitalPayload(name: "FROZEN_FRAMES", value: Double(frozen), rating: nil),
        ]
    }
}
