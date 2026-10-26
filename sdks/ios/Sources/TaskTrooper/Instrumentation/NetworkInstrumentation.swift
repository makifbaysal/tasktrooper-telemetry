import Foundation

/// URLSession instrumentation.
///
/// Approach: swizzle `-[NSURLSessionTask resume]` on the concrete task classes and observe the
/// task's `state` via KVO until `.completed`. At resume time `traceparent` (+ `baggage: tt.svc`) is added to the
/// task's current request (private `setCurrentRequest:`, guarded by `responds(to:)`).
///
/// Why not `URLProtocol`: a protocol re-issues requests itself, only applies to sessions whose
/// configuration lists it (needs extra swizzling of `URLSessionConfiguration`), breaks background
/// sessions, auth challenges, task metrics and delegates' expectations. Why not delegate swizzling:
/// completion-handler and async/await calls have no delegate. Resume + KVO sees every task type
/// (data/upload/download, completion handler, delegate, async/await) without changing request
/// semantics; trade-off: relies on a private setter for header injection (skipped if missing) and
/// durations are measured resume→completed rather than from `URLSessionTaskMetrics`.
final class NetworkInstrumentation: @unchecked Sendable {
    struct Record: Sendable {
        var url: URL
        var method: String
        var statusCode: Int
        var startMs: Int64
        var durationMs: Double
        var requestBytes: Int64
        var responseBytes: Int64
        var traceId: String?
        var spanId: String?
        var errorDescription: String?
        var errorType: String?
        var cancelled: Bool
    }

    struct Handler: Sendable {
        var propagation: TracePropagation
        var shouldPropagate: @Sendable () -> Bool
        var onComplete: @Sendable (Record) -> Void
        var clock: TrooperClock = .system
        /// App name sent as `baggage: tt.svc=<app>` next to `traceparent`.
        var serviceName: String? = nil
    }

    private struct Pending {
        var observation: NSKeyValueObservation?
        var url: URL
        var method: String
        var startUptime: TimeInterval
        var startMs: Int64
        var traceId: String?
        var spanId: String?
    }

    static let shared = NetworkInstrumentation()
    static let sdkHeader = "x-trooper-key"

    private let handler = Locked<Handler?>(nil)
    private let pending = Locked<[ObjectIdentifier: Pending]>([:])
    private let installed = Locked(false)

    func enable(_ h: Handler) {
        handler.set(h)
        installOnce()
    }

    func disable() { handler.set(nil) }

    private func installOnce() {
        let first = installed.withLock { (v: inout Bool) -> Bool in
            defer { v = true }
            return !v
        }
        guard first else { return }
        let session = URLSession(configuration: .ephemeral)
        guard let url = URL(string: "https://127.0.0.1/") else { return }
        let samples: [URLSessionTask] = [
            session.dataTask(with: url),
            session.downloadTask(with: url),
            session.uploadTask(with: URLRequest(url: url), from: Data()),
        ]
        var done = Set<ObjectIdentifier>()
        let resume = NSSelectorFromString("resume")
        for task in samples {
            var cls: AnyClass? = object_getClass(task)
            while let c = cls, c != NSObject.self {
                if !done.contains(ObjectIdentifier(c)), Self.classDefines(c, resume) {
                    done.insert(ObjectIdentifier(c))
                    Self.swizzleResume(c, resume)
                }
                cls = class_getSuperclass(c)
            }
            task.cancel()
        }
        session.invalidateAndCancel()
    }

    private static func classDefines(_ cls: AnyClass, _ sel: Selector) -> Bool {
        var count: UInt32 = 0
        guard let list = class_copyMethodList(cls, &count) else { return false }
        defer { free(list) }
        for i in 0..<Int(count) where method_getName(list[i]) == sel { return true }
        return false
    }

    private static func swizzleResume(_ cls: AnyClass, _ sel: Selector) {
        guard let method = class_getInstanceMethod(cls, sel) else { return }
        typealias Fn = @convention(c) (AnyObject, Selector) -> Void
        let original = unsafeBitCast(method_getImplementation(method), to: Fn.self)
        let block: @convention(block) (AnyObject) -> Void = { obj in
            if let task = obj as? URLSessionTask {
                NetworkInstrumentation.shared.willResume(task)
            }
            original(obj, sel)
        }
        method_setImplementation(method, imp_implementationWithBlock(block))
    }

    func willResume(_ task: URLSessionTask) {
        guard let h = handler.current, task.state == .suspended else { return }
        let key = ObjectIdentifier(task)
        if pending.current[key] != nil { return }
        guard let request = task.currentRequest ?? task.originalRequest,
              let url = request.url,
              let scheme = url.scheme?.lowercased(), scheme == "http" || scheme == "https",
              request.value(forHTTPHeaderField: Self.sdkHeader) == nil
        else { return }

        var traceId: String?
        var spanId: String?
        if h.propagation.matches(url), h.shouldPropagate() {
            let setter = NSSelectorFromString("setCurrentRequest:")
            if task.responds(to: setter) {
                var mutable = request
                var changed = false
                if request.value(forHTTPHeaderField: "traceparent") == nil {
                    let t = IDs.traceId(), s = IDs.spanId()
                    mutable.setValue(TracePropagation.header(traceId: t, spanId: s), forHTTPHeaderField: "traceparent")
                    traceId = t
                    spanId = s
                    changed = true
                }
                let existing = request.value(forHTTPHeaderField: "baggage")
                if let merged = TracePropagation.mergeBaggage(existing, service: h.serviceName), merged != existing {
                    mutable.setValue(merged, forHTTPHeaderField: "baggage")
                    changed = true
                }
                if changed { task.perform(setter, with: mutable as NSURLRequest) }
            }
        }

        let entry = Pending(observation: nil, url: url, method: request.httpMethod ?? "GET",
                            startUptime: h.clock.uptime(), startMs: h.clock.nowMs(), traceId: traceId, spanId: spanId)
        pending.withLock { $0[key] = entry }
        let observation = task.observe(\.state, options: [.new]) { t, _ in
            NetworkInstrumentation.shared.stateChanged(t)
        }
        let stillPending = pending.withLock { (map: inout [ObjectIdentifier: Pending]) -> Bool in
            guard map[key] != nil else { return false }
            map[key]?.observation = observation
            return true
        }
        if !stillPending { observation.invalidate() }
    }

    private func stateChanged(_ task: URLSessionTask) {
        guard task.state == .completed else { return }
        let key = ObjectIdentifier(task)
        guard let p = pending.withLock({ $0.removeValue(forKey: key) }) else { return }
        p.observation?.invalidate()
        guard let h = handler.current else { return }
        let http = task.response as? HTTPURLResponse
        let error = task.error as NSError?
        let record = Record(
            url: p.url,
            method: p.method,
            statusCode: http?.statusCode ?? 0,
            startMs: p.startMs,
            durationMs: max(0, (h.clock.uptime() - p.startUptime) * 1000),
            requestBytes: task.countOfBytesSent,
            responseBytes: task.countOfBytesReceived,
            traceId: p.traceId,
            spanId: p.spanId,
            errorDescription: error?.localizedDescription,
            errorType: error.map { "\($0.domain)(\($0.code))" },
            cancelled: error?.domain == NSURLErrorDomain && error?.code == NSURLErrorCancelled
        )
        h.onComplete(record)
    }
}
