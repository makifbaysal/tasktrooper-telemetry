import Foundation

enum IDs {
    static func uuid() -> String { UUID().uuidString.lowercased() }

    /// Random lowercase hex of `bytes` length; never all zeros (invalid in W3C trace context).
    static func hex(bytes: Int) -> String {
        var rng = SystemRandomNumberGenerator()
        while true {
            let raw = (0..<bytes).map { _ in UInt8.random(in: 0...255, using: &rng) }
            if raw.contains(where: { $0 != 0 }) {
                return raw.map { String(format: "%02x", $0) }.joined()
            }
        }
    }

    static func traceId() -> String { hex(bytes: 16) }
    static func spanId() -> String { hex(bytes: 8) }
}
