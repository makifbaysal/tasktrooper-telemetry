import Foundation

/// Session rotation: new session after 30 min of inactivity or 4 h total. Persisted in UserDefaults so
/// a quick relaunch continues the same session. Confined to the SDK queue.
final class SessionManager {
    struct Session: Codable, Equatable {
        var id: String
        var startMs: Int64
        var lastActivityMs: Int64
        var sampled: Bool
    }

    static let inactivityMs: Int64 = 30 * 60 * 1000
    static let maxDurationMs: Int64 = 4 * 60 * 60 * 1000
    private static let storageKey = "dev.tasktrooper.session"

    private let defaults: UserDefaults
    private let sampleRate: Double
    private let random: () -> Double
    private(set) var current: Session?
    private var lastPersistMs: Int64 = 0

    init(defaults: UserDefaults, sampleRate: Double, random: @escaping () -> Double = { Double.random(in: 0..<1) }) {
        self.defaults = defaults
        self.sampleRate = min(max(sampleRate, 0), 1)
        self.random = random
        if let data = defaults.data(forKey: Self.storageKey),
           let s = try? JSONDecoder().decode(Session.self, from: data) {
            current = s
        }
    }

    /// Records activity at `nowMs`; returns the active session and whether it was just created.
    @discardableResult
    func touch(nowMs: Int64) -> (session: Session, isNew: Bool) {
        if var s = current, !isExpired(s, nowMs: nowMs) {
            s.lastActivityMs = max(s.lastActivityMs, nowMs)
            current = s
            if nowMs - lastPersistMs > 10_000 { persist() }
            return (s, false)
        }
        let s = Session(id: IDs.uuid(), startMs: nowMs, lastActivityMs: nowMs, sampled: random() < sampleRate)
        current = s
        persist()
        return (s, true)
    }

    func isExpired(_ s: Session, nowMs: Int64) -> Bool {
        nowMs - s.lastActivityMs >= Self.inactivityMs || nowMs - s.startMs >= Self.maxDurationMs
    }

    func persist() {
        guard let s = current, let data = try? JSONEncoder().encode(s) else { return }
        defaults.set(data, forKey: Self.storageKey)
        lastPersistMs = s.lastActivityMs
    }
}

enum AnonymousID {
    static let storageKey = "dev.tasktrooper.anonymous_id"

    static func load(_ defaults: UserDefaults) -> String {
        if let id = defaults.string(forKey: storageKey), !id.isEmpty { return id }
        let id = IDs.uuid()
        defaults.set(id, forKey: storageKey)
        return id
    }
}
