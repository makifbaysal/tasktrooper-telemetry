import Foundation

enum TrooperLog {
    static let enabled = Locked(false)

    static func debug(_ message: @autoclosure () -> String) {
        guard enabled.current else { return }
        NSLog("[TaskTrooper] %@", message())
    }
}
