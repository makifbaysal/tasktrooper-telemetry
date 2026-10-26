import Foundation
import Network

/// Connection type for the `connection` field and offline detection for the uploader.
final class Reachability: @unchecked Sendable {
    private let monitor = NWPathMonitor()
    private let queue = DispatchQueue(label: "dev.tasktrooper.reachability", qos: .utility)
    let connection = Locked("unknown")

    func start(onChange: @escaping @Sendable (String) -> Void) {
        monitor.pathUpdateHandler = { [connection] path in
            let value: String
            if path.status != .satisfied {
                value = "offline"
            } else if path.usesInterfaceType(.wifi) {
                value = "wifi"
            } else if path.usesInterfaceType(.cellular) {
                value = "cellular"
            } else {
                value = "unknown"
            }
            let changed = connection.withLock { (c: inout String) -> Bool in
                defer { c = value }
                return c != value
            }
            if changed { onChange(value) }
        }
        monitor.start(queue: queue)
    }
}
