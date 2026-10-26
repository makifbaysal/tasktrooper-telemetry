import Foundation

/// Stack of visible screens. The top entry is the current view. When the top disappears, the screen
/// below (still on screen, e.g. the presenter of a dismissed sheet) becomes current again as a new
/// view instance. Pure value type; confined to the SDK queue.
struct ViewStack {
    struct Entry: Equatable {
        var key: String
        var name: String
        var id: String
        var startMs: Int64
        var attributes: [String: String]
    }

    static let maxDepth = 50
    private(set) var entries: [Entry] = []
    private(set) var suspended = false

    var current: Entry? { suspended ? nil : entries.last }

    /// Returns the view that ended (if any) and the started one (nil when `key` is already current).
    mutating func start(key: String, name: String, id: String, ts: Int64, attributes: [String: String] = [:]) -> (ended: Entry?, started: Entry?) {
        if !suspended, let top = entries.last, top.key == key { return (nil, nil) }
        let ended = current
        suspended = false
        entries.removeAll { $0.key == key }
        let e = Entry(key: key, name: name, id: id, startMs: ts, attributes: attributes)
        entries.append(e)
        if entries.count > Self.maxDepth { entries.removeFirst(entries.count - Self.maxDepth) }
        return (ended, e)
    }

    mutating func stop(key: String, ts: Int64, newId: () -> String) -> (ended: Entry?, resumed: Entry?) {
        guard let idx = entries.lastIndex(where: { $0.key == key }) else { return (nil, nil) }
        let wasTop = idx == entries.count - 1
        let removed = entries.remove(at: idx)
        guard wasTop, !suspended else { return (nil, nil) }
        guard var top = entries.popLast() else { return (removed, nil) }
        top.id = newId()
        top.startMs = ts
        entries.append(top)
        return (removed, top)
    }

    /// App went to background: end the current view, keep the stack.
    mutating func suspend() -> Entry? {
        let c = current
        suspended = true
        return c
    }

    /// App back in foreground: restart the top view as a new instance.
    mutating func resume(ts: Int64, newId: () -> String) -> Entry? {
        guard suspended else { return nil }
        suspended = false
        guard var top = entries.popLast() else { return nil }
        top.id = newId()
        top.startMs = ts
        entries.append(top)
        return top
    }
}
