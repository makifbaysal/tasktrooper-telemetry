import Foundation

/// Buffers events and hands them off as a batch when 100 events accumulate or 5 s pass since the
/// first buffered event (`tick` is driven by a timer, or manually in tests). Confined to the SDK queue.
final class EventBatcher {
    private let maxBatchSize: Int
    private let maxAgeMs: Int64
    private let sink: ([RumEvent]) -> Void
    private(set) var buffer: [RumEvent] = []
    private var firstEventMs: Int64?

    init(maxBatchSize: Int = 100, maxAge: TimeInterval = 5, sink: @escaping ([RumEvent]) -> Void) {
        self.maxBatchSize = max(1, maxBatchSize)
        self.maxAgeMs = Int64(maxAge * 1000)
        self.sink = sink
    }

    func add(_ event: RumEvent, nowMs: Int64) {
        if buffer.isEmpty { firstEventMs = nowMs }
        buffer.append(event)
        if buffer.count >= maxBatchSize { flush() }
    }

    /// Flushes if the oldest buffered event is at least `maxAge` old. Returns true if flushed.
    @discardableResult
    func tick(nowMs: Int64) -> Bool {
        guard let first = firstEventMs, !buffer.isEmpty, nowMs - first >= maxAgeMs else { return false }
        flush()
        return true
    }

    func flush() {
        guard !buffer.isEmpty else { return }
        let events = buffer
        buffer = []
        firstEventMs = nil
        sink(events)
    }
}
