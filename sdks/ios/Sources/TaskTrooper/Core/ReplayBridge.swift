import Foundation

/// SDK-internal bridge for the `TaskTrooperReplay` target (session replay, contracts.md §23): RUM session /
/// view / user, ingest endpoint, error notifications and gzip. Not a public API: may change without notice.
/// Every member is safe before `TaskTrooper.start` (nil / no-op).
@_spi(TrooperReplay)
public enum TrooperReplayBridge {
    public struct Context: Sendable, Equatable {
        public var sessionId: String
        /// False when the RUM session is not sampled: replay must not record either.
        public var sampled: Bool
        public var anonymousId: String
        public var userId: String?
        public var viewId: String?
        public var viewName: String?

        public init(sessionId: String, sampled: Bool, anonymousId: String, userId: String? = nil,
                    viewId: String? = nil, viewName: String? = nil) {
            self.sessionId = sessionId
            self.sampled = sampled
            self.anonymousId = anonymousId
            self.userId = userId
            self.viewId = viewId
            self.viewName = viewName
        }
    }

    public struct Resource: Sendable, Equatable {
        public var service: String
        public var environment: String
        public var version: String
    }

    public static var isStarted: Bool { TaskTrooper.isStarted }

    /// Current RUM context. Blocks on the SDK queue briefly: never call from the main thread.
    public static func context() -> Context? {
        guard let c = TaskTrooper.core.current else { return nil }
        return c.queue.sync {
            if let x = c.externalContext {
                // Hybrid app: the JS/Dart layer owns the session.
                return Context(sessionId: x.sessionId, sampled: c.sessionSampled.current, anonymousId: x.anonymousId,
                               userId: x.userId, viewId: x.viewId, viewName: x.viewName)
            }
            let s = c.replaySession()
            let v = c.currentViewSnapshot()
            return Context(sessionId: s.id, sampled: s.sampled, anonymousId: c.anonymousId,
                           userId: c.currentUserSnapshot()?.id, viewId: v?.id, viewName: v?.name)
        }
    }

    /// `POST /v1/replay` URL and ingest key from the DSN.
    public static var ingest: (url: URL, key: String)? {
        guard let c = TaskTrooper.core.current else { return nil }
        return (c.dsn.baseURL.appendingPathComponent("v1/replay"), c.dsn.key)
    }

    public static var resource: Resource? {
        guard let c = TaskTrooper.core.current else { return nil }
        return Resource(service: c.config.app, environment: c.config.environment, version: c.config.version)
    }

    /// `observer(tsMs)` runs on the SDK queue after an ERROR (handled, hang, network) of the current sampled
    /// session is recorded. Crashes are reported on the next launch and do not trigger it. Returns false before start.
    @discardableResult
    public static func addErrorObserver(_ observer: @escaping @Sendable (Int64) -> Void) -> Bool {
        guard let c = TaskTrooper.core.current else { return false }
        c.errorObservers.withLock { $0.append(observer) }
        return true
    }

    public static func gzip(_ data: Data) -> Data? { Gzip.compress(data) }
    public static func gunzip(_ data: Data) -> Data? { Gzip.decompress(data) }

    public static var isDebug: Bool { TrooperLog.enabled.current }
    public static func log(_ message: @autoclosure () -> String) { TrooperLog.debug(message()) }

    /// Retry / drop decision for an upload response (contracts.md §2), shared with the RUM uploader.
    public enum Outcome: Equatable, Sendable { case success, drop, retry(after: TimeInterval?) }

    public static func classify(statusCode: Int?, retryAfter: String?, hasError: Bool) -> Outcome {
        switch UploadOutcome.classify(statusCode: statusCode, retryAfter: retryAfter, hasError: hasError) {
        case .success: return .success
        case .drop: return .drop
        case .retry(let after): return .retry(after: after)
        }
    }

    /// Backoff delay (1 s → 60 s, jittered) for the n-th consecutive failure.
    public static func backoffDelay(attempt: Int) -> TimeInterval { Backoff().delay(attempt: attempt) }
}
