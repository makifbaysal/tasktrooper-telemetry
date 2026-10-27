import Foundation
#if canImport(TaskTrooperCrashC)
import TaskTrooperCrashC
#endif

/// Context shared with a JS / Dart RUM SDK running in the same app (contracts.md §9).
/// - Flutter: the `trooper_flutter` plugin registers the MethodChannel `dev.tasktrooper.telemetry/rum`.
/// - React Native: the native module is named `TrooperRum` (the JS side falls back to a module named
///   `dev.tasktrooper.telemetry/rum`).
/// Both map `setContext` / `getContext` / `getProcessStartTime` onto the `TaskTrooper` methods below.
public struct TrooperHybridContext: Sendable, Equatable {
    public var sessionId: String
    public var anonymousId: String
    public var viewId: String?
    public var viewName: String?
    public var userId: String?
    /// RUM sampling decision of the JS/Dart layer for this session (`setContext` `sampled`). nil = not sent: the
    /// native decision current when the session was adopted is kept for it.
    public var sampled: Bool?

    public init(sessionId: String, anonymousId: String, viewId: String? = nil, viewName: String? = nil, userId: String? = nil,
                sampled: Bool? = nil) {
        self.sampled = sampled
        self.sessionId = sessionId
        self.anonymousId = anonymousId
        self.viewId = viewId
        self.viewName = viewName
        self.userId = userId
    }

    /// Dictionary form used on the channel (snake_case keys).
    public var dictionary: [String: String] {
        var d = ["session_id": sessionId, "anonymous_id": anonymousId]
        if let viewId { d["view_id"] = viewId }
        if let viewName { d["view_name"] = viewName }
        if let userId { d["user_id"] = userId }
        return d
    }

    public init?(dictionary d: [String: Any]) {
        guard let s = d["session_id"] as? String, !s.isEmpty else { return nil }
        self.init(sessionId: s, anonymousId: d["anonymous_id"] as? String ?? "", viewId: d["view_id"] as? String,
                  viewName: d["view_name"] as? String, userId: d["user_id"] as? String,
                  sampled: d["sampled"] as? Bool)
    }
}

extension TaskTrooper {
    /// `getContext()`: the adopted context once `setContext` was called, else the native session (nil before `start`).
    public static func hybridContext() -> TrooperHybridContext? {
        core.current?.nativeContext()
    }

    /// `setContext(...)`: the JS/Dart layer owns session/view. The adopted session is external (contracts.md §9):
    /// native events, crash reports and session replay use it, it never gets a native SESSION_START and native
    /// inactivity / 4 h rules never rotate it. `sampled` (when sent) is its RUM sampling decision. nil = hand back.
    public static func setHybridContext(_ context: TrooperHybridContext?) {
        core.current?.setExternalContext(context)
    }

    /// `getProcessStartTime()`: unix epoch ms of process start (sysctl), for APP_START `start_reference=process`.
    public static func processStartTimeMs() -> Int64 {
        tt_process_start_time_ms()
    }
}
