import Foundation

/// SDK state. Everything mutable is confined to `queue` (serial); public entry points hop onto it.
final class TrooperCore: @unchecked Sendable {
    struct Environment {
        var clock: TrooperClock = .system
        var defaults: UserDefaults = .standard
        var storageDirectory: URL? = DiskQueue.defaultDirectory()
        var urlSession: URLSession = URLSession(configuration: .ephemeral)
        var backoff = Backoff()
        var uploadScheduler: Uploader.Scheduler?
        var random: () -> Double = { Double.random(in: 0..<1) }
        var startTimer = true
    }

    let config: TrooperConfig
    let dsn: TrooperDSN
    let clock: TrooperClock
    let queue = DispatchQueue(label: "dev.tasktrooper.core", qos: .utility)
    let storageDirectory: URL
    let anonymousId: String

    let connection = Locked("unknown")
    let sessionSampled = Locked(true)
    let frameStats = Locked(FrameStats())

    // queue-confined
    private(set) var resource: RumResource
    private let sessions: SessionManager
    private var user: RumUser?
    private var views = ViewStack()
    private var viewsInSession = 0
    private var batcher: EventBatcher!
    let disk: DiskQueue
    let uploader: Uploader
    private var timer: DispatchSourceTimer?

    init?(config: TrooperConfig, environment env: Environment = Environment()) {
        guard let dsn = TrooperDSN(config.dsn) else { return nil }
        self.config = config
        self.dsn = dsn
        self.clock = env.clock
        let dir = env.storageDirectory ?? FileManager.default.temporaryDirectory.appendingPathComponent("TaskTrooper", isDirectory: true)
        self.storageDirectory = dir
        guard let disk = DiskQueue(directory: dir.appendingPathComponent("queue", isDirectory: true), maxBytes: config.maxQueueBytes)
        else { return nil }
        self.disk = disk
        self.anonymousId = AnonymousID.load(env.defaults)
        self.sessions = SessionManager(defaults: env.defaults, sampleRate: config.sampleRate, random: env.random)
        self.resource = RumResource(
            app: config.app, version: config.version, environment: config.environment, platform: .ios,
            sdk: RumSdk(name: DeviceInfo.sdkName, version: DeviceInfo.sdkVersion),
            device: DeviceInfo.baseDevice(), os: RumOs(name: DeviceInfo.osName(), version: DeviceInfo.osVersion()),
            attributes: config.resourceAttributes.isEmpty ? nil : config.resourceAttributes)
        self.uploader = Uploader(queue: queue, disk: disk, endpoint: dsn.rumURL, key: dsn.key, session: env.urlSession,
                                 backoff: env.backoff, clock: env.clock, schedule: env.uploadScheduler)
        self.batcher = EventBatcher(maxBatchSize: config.maxBatchSize, maxAge: config.flushInterval) { [unowned self] events in
            self.persist(events)
        }
        uploader.isOnline = { [connection] in connection.current != "offline" }
        if let s = sessions.current { sessionSampled.set(s.sampled) }
        if env.startTimer { queue.async { self.startTimer() } }
    }

    // MARK: - Pipeline

    private func startTimer() {
        let t = DispatchSource.makeTimerSource(queue: queue)
        t.schedule(deadline: .now() + 1, repeating: 1, leeway: .milliseconds(200))
        t.setEventHandler { [weak self] in self?.tick() }
        t.resume()
        timer = t
        uploader.drain()  // leftovers from previous launches
    }

    /// Timer body (also called by tests).
    func tick() {
        batcher.tick(nowMs: clock.nowMs())
    }

    private func persist(_ events: [RumEvent]) {
        let batch = RumBatch(resource: resource, events: events)
        guard let json = try? RumJSON.encoder().encode(batch), let gz = Gzip.compress(json) else { return }
        disk.enqueue(gz, nowMs: clock.nowMs())
        uploader.drain()
    }

    /// Flushes buffered events and uploads everything queued; `completion` runs on the SDK queue.
    func flush(completion: (@Sendable () -> Void)? = nil) {
        queue.async {
            self.batcher.flush()
            self.uploader.drain { completion?() }
        }
    }

    // MARK: - Events (queue-confined)

    private func base(_ type: RumEventType, ts: Int64, session: SessionManager.Session) -> RumEvent {
        let v = views.current
        return RumEvent(type: type, tsMs: ts, sessionId: session.id, viewId: v?.id, viewName: v?.name,
                        anonymousId: anonymousId, user: user, connection: connection.current)
    }

    private func currentSession(ts: Int64, touch: Bool) -> SessionManager.Session {
        if !touch, let s = sessions.current { return s }
        let (session, isNew) = sessions.touch(nowMs: ts)
        if isNew {
            sessionSampled.set(session.sampled)
            viewsInSession = 0
            updateCrashContext()
            if session.sampled { append(base(.sessionStart, ts: ts, session: session)) }
        }
        return session
    }

    /// Builds and buffers one event. Must run on `queue`.
    func emit(_ type: RumEventType, ts: Int64, touchSession: Bool = true, _ fill: (inout RumEvent) -> Void) {
        let session = currentSession(ts: ts, touch: touchSession)
        guard session.sampled else { return }
        var e = base(type, ts: ts, session: session)
        fill(&e)
        append(e)
    }

    private func append(_ event: RumEvent) {
        var e = event
        if let hook = config.beforeSend {
            guard let modified = hook(e) else { return }
            e = modified
        }
        batcher.add(e, nowMs: clock.nowMs())
    }

    /// Thread-safe entry point.
    func submit(_ type: RumEventType, ts: Int64? = nil, _ fill: @escaping @Sendable (inout RumEvent) -> Void) {
        let t = ts ?? clock.nowMs()
        queue.async { self.emit(type, ts: t, fill) }
    }

    // MARK: - Views

    func startView(name: String, key: String, attributes: [String: String] = [:], loadingTimeMs: Double? = nil) {
        let ts = clock.nowMs()
        queue.async {
            let (ended, started) = self.views.start(key: key, name: name, id: IDs.uuid(), ts: ts, attributes: attributes)
            if let ended { self.emitViewEnd(ended, ts: ts) }
            if let started { self.emitViewStart(started, ts: ts, loadingTimeMs: loadingTimeMs) }
        }
    }

    func stopView(key: String) {
        let ts = clock.nowMs()
        queue.async {
            let (ended, resumed) = self.views.stop(key: key, ts: ts, newId: IDs.uuid)
            if let ended { self.emitViewEnd(ended, ts: ts) }
            if let resumed { self.emitViewStart(resumed, ts: ts, loadingTimeMs: nil) }
            if ended != nil || resumed != nil { self.updateCrashContext() }
        }
    }

    /// App entered background: end the visible view and flush.
    func enterBackground(completion: (@Sendable () -> Void)? = nil) {
        let ts = clock.nowMs()
        queue.async {
            if let ended = self.views.suspend() { self.emitViewEnd(ended, ts: ts) }
            self.sessions.persist()
            self.batcher.flush()
            self.uploader.drain { completion?() }
        }
    }

    func enterForeground() {
        let ts = clock.nowMs()
        queue.async {
            if let resumed = self.views.resume(ts: ts, newId: IDs.uuid) { self.emitViewStart(resumed, ts: ts, loadingTimeMs: nil) }
        }
    }

    private func emitViewStart(_ v: ViewStack.Entry, ts: Int64, loadingTimeMs: Double?) {
        frameStats.set(FrameStats())
        let session = currentSession(ts: ts, touch: true)
        let initial = viewsInSession == 0
        viewsInSession += 1
        guard session.sampled else { return updateCrashContext() }
        var e = base(.viewStart, ts: ts, session: session)
        e.viewId = v.id
        e.viewName = v.name
        e.view = RumViewPayload(url: nil, referrer: nil, loadingTimeMs: loadingTimeMs, timeSpentMs: nil, isInitial: initial)
        e.attributes = v.attributes.isEmpty ? nil : v.attributes
        append(e)
        updateCrashContext()
    }

    private func emitViewEnd(_ v: ViewStack.Entry, ts: Int64) {
        let stats = frameStats.withLock { (s: inout FrameStats) -> FrameStats in
            defer { s = FrameStats() }
            return s
        }
        let session = currentSession(ts: ts, touch: true)
        guard session.sampled else { return }
        var e = base(.viewEnd, ts: ts, session: session)
        e.viewId = v.id
        e.viewName = v.name
        e.view = RumViewPayload(url: nil, referrer: nil, loadingTimeMs: nil, timeSpentMs: Double(max(0, ts - v.startMs)), isInitial: nil)
        append(e)
        for vital in stats.vitals() {
            var ve = base(.vital, ts: ts, session: session)
            ve.viewId = v.id
            ve.viewName = v.name
            ve.vital = vital
            ve.attributes = ["total_frames": String(stats.total)]
            append(ve)
        }
    }

    // MARK: - User

    func identify(_ user: RumUser?) {
        queue.async {
            self.user = user
            self.updateCrashContext()
        }
    }

    // MARK: - Crash / device context

    var externalContext: TrooperHybridContext?

    func currentSessionSnapshot() -> SessionManager.Session? { sessions.current }
    func currentViewSnapshot() -> ViewStack.Entry? { views.current }
    func currentUserSnapshot() -> RumUser? { user }
    func refreshCrashContext() { updateCrashContext() }

    private func updateCrashContext() {
        guard config.trackCrashes else { return }
        if let x = externalContext {
            CrashReporter.setContext(sessionId: x.sessionId, viewId: x.viewId, viewName: x.viewName, userId: x.userId)
            return
        }
        guard let s = sessions.current else { return }
        let v = views.current
        CrashReporter.setContext(sessionId: s.sampled ? s.id : CrashReporter.unsampledSession,
                                 viewId: v?.id, viewName: v?.name, userId: user?.id)
    }

    /// Queues a crash from a previous process, attributed to the session/view it happened in.
    func emitCrash(_ report: CrashReport) {
        guard report.sessionId != CrashReporter.unsampledSession else { return }
        queue.async {
            self.emit(.error, ts: report.tsMs, touchSession: false) { e in
                if !report.sessionId.isEmpty { e.sessionId = report.sessionId }
                e.viewId = report.viewId.isEmpty ? nil : report.viewId
                e.viewName = report.viewName.isEmpty ? nil : report.viewName
                e.user = report.userId.isEmpty ? nil : RumUser(id: report.userId)
                e.error = report.errorPayload()
                var attrs = ["crash.signal": report.signalName, "crash.complete": report.complete ? "true" : "false",
                             "crash.threads": String(report.threads.count + 1)]
                if let info = report.crashInfoMessage, !info.isEmpty { attrs["crash.info"] = String(info.prefix(1024)) }
                e.attributes = attrs
            }
            self.batcher.flush()
        }
    }

    func updateDevice(_ device: RumDevice) {
        queue.async { self.resource.device = device }
    }

    // MARK: - Network

    func recordResource(_ r: NetworkInstrumentation.Record) {
        submit(.resource, ts: r.startMs) { e in
            e.resource = RumResourcePayload(
                url: URLSanitizer.sanitize(r.url), method: r.method, statusCode: Int32(clamping: r.statusCode),
                durationMs: r.durationMs, requestBytes: r.requestBytes, responseBytes: r.responseBytes,
                traceId: r.traceId, spanId: r.spanId, initiator: "urlsession")
        }
        if let desc = r.errorDescription, !r.cancelled {
            submit(.error, ts: r.startMs + Int64(r.durationMs)) { e in
                e.error = RumErrorPayload(type: r.errorType ?? "NetworkError", message: desc, stack: nil, source: "network",
                                          isCrash: false, thread: nil, fingerprint: nil)
                e.attributes = ["http.method": r.method, "http.url": URLSanitizer.sanitize(r.url)]
            }
        }
    }
}

extension TrooperCore {
    /// Starts (or continues) the session and publishes crash context.
    func prime() {
        queue.async {
            _ = self.currentSession(ts: self.clock.nowMs(), touch: true)
            self.updateCrashContext()
        }
    }
}

extension TrooperCore {
    func nativeContext() -> TrooperHybridContext? {
        queue.sync {
            let s = currentSessionSnapshot()
            guard let s else { return nil }
            let v = currentViewSnapshot()
            return TrooperHybridContext(sessionId: s.id, anonymousId: anonymousId, viewId: v?.id, viewName: v?.name, userId: currentUserSnapshot()?.id)
        }
    }

    func setExternalContext(_ ctx: TrooperHybridContext?) {
        queue.async {
            self.externalContext = ctx
            self.refreshCrashContext()
        }
    }
}
