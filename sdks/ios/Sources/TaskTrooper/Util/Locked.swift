import Foundation

/// Minimal lock-protected box used for state shared between the SDK queue, the main thread and
/// instrumentation callbacks that run on arbitrary threads.
final class Locked<Value>: @unchecked Sendable {
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

    func set(_ newValue: Value) { withLock { $0 = newValue } }
}
