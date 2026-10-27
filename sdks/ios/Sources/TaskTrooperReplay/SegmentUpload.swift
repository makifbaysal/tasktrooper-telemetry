import Foundation
#if SWIFT_PACKAGE
@_spi(TrooperReplay) import TaskTrooper
#endif

/// Offline segment store: one gzipped segment per file. A segment that would push the directory past
/// `maxBytes` is dropped (replay is optional data; the disk stays small). Confined to the replay queue.
final class SegmentStore {
    let directory: URL
    let maxBytes: Int
    private let fm = FileManager.default
    private var seq = 0
    private(set) var dropped = 0

    init?(directory: URL, maxBytes: Int) {
        self.directory = directory
        self.maxBytes = maxBytes
        do {
            try fm.createDirectory(at: directory, withIntermediateDirectories: true)
            var dir = directory
            var values = URLResourceValues()
            values.isExcludedFromBackup = true
            try? dir.setResourceValues(values)
        } catch {
            return nil
        }
    }

    static func defaultDirectory() -> URL? {
        FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first?
            .appendingPathComponent("TaskTrooper/replay", isDirectory: true)
    }

    @discardableResult
    func write(_ gz: Data, nowMs: Int64) -> Bool {
        guard totalBytes + gz.count <= maxBytes else {
            dropped += 1
            TrooperReplayBridge.log("replay: offline store full, segment dropped")
            return false
        }
        seq = (seq + 1) % 1_000_000
        let url = directory.appendingPathComponent(String(format: "%013lld-%06d.seg.gz", nowMs, seq))
        do { try gz.write(to: url, options: .atomic) } catch { return false }
        return true
    }

    func files() -> [(url: URL, size: Int)] {
        let urls = (try? fm.contentsOfDirectory(at: directory, includingPropertiesForKeys: [.fileSizeKey])) ?? []
        return urls.filter { $0.lastPathComponent.hasSuffix(".seg.gz") }
            .sorted { $0.lastPathComponent < $1.lastPathComponent }
            .map { ($0, (try? $0.resourceValues(forKeys: [.fileSizeKey]).fileSize) ?? 0) }
    }

    func oldest() -> URL? { files().first?.url }
    var totalBytes: Int { files().reduce(0) { $0 + $1.size } }
    func remove(_ url: URL) { try? fm.removeItem(at: url) }
}

/// Uploads stored segments oldest-first to `POST /v1/replay` (gzip, `x-trooper-key`); 2xx/4xx delete,
/// 429/5xx/network keep and back off (contracts.md §2). Confined to `queue`.
final class SegmentUploader: @unchecked Sendable {
    typealias Scheduler = (TimeInterval, @escaping @Sendable () -> Void) -> Void

    private let queue: DispatchQueue
    private let store: SegmentStore
    private let session: URLSession
    private let endpoint: () -> (url: URL, key: String)?
    private let schedule: Scheduler
    private var sending = false
    private var failures = 0
    private var retryScheduled = false
    private var waiters: [() -> Void] = []
    private(set) var lastDelay: TimeInterval?

    init(queue: DispatchQueue, store: SegmentStore, session: URLSession,
         endpoint: @escaping () -> (url: URL, key: String)? = { TrooperReplayBridge.ingest },
         schedule: Scheduler? = nil) {
        self.queue = queue
        self.store = store
        self.session = session
        self.endpoint = endpoint
        self.schedule = schedule ?? { delay, work in queue.asyncAfter(deadline: .now() + delay, execute: work) }
    }

    func drain(_ done: (() -> Void)? = nil) {
        if let done { waiters.append(done) }
        guard !sending, !retryScheduled, let file = store.oldest(), let target = endpoint() else { return finish() }
        guard let body = try? Data(contentsOf: file), !body.isEmpty else {
            store.remove(file)
            return drain()
        }
        var req = URLRequest(url: target.url)
        req.httpMethod = "POST"
        req.timeoutInterval = 30
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.setValue("gzip", forHTTPHeaderField: "Content-Encoding")
        req.setValue(target.key, forHTTPHeaderField: "x-trooper-key")
        sending = true
        session.uploadTask(with: req, from: body) { [self] _, response, error in
            let http = response as? HTTPURLResponse
            let status = http?.statusCode
            let retryAfter = http?.value(forHTTPHeaderField: "Retry-After")
            queue.async { self.handle(file: file, status: status, retryAfter: retryAfter, error: error) }
        }.resume()
    }

    private func handle(file: URL, status: Int?, retryAfter: String?, error: Error?) {
        sending = false
        switch TrooperReplayBridge.classify(statusCode: status, retryAfter: retryAfter, hasError: error != nil) {
        case .success:
            failures = 0
            store.remove(file)
            drain()
        case .drop:
            TrooperReplayBridge.log("replay segment rejected with HTTP \(status ?? 0), dropped")
            failures = 0
            store.remove(file)
            drain()
        case .retry(let after):
            failures += 1
            let delay = after ?? TrooperReplayBridge.backoffDelay(attempt: failures)
            lastDelay = delay
            retryScheduled = true
            schedule(delay) { [self] in
                queue.async {
                    self.retryScheduled = false
                    self.drain()
                }
            }
            finish()
        }
    }

    private func finish() {
        let w = waiters
        waiters = []
        w.forEach { $0() }
    }
}
