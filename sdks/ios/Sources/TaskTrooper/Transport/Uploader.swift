import Foundation

/// Drains the disk queue one batch at a time: `POST /v1/rum`, gzip, `x-trooper-key`.
/// 2xx → delete, 4xx (≠429) → delete (drop), 429/5xx/network → keep + backoff. Confined to `queue`.
final class Uploader: @unchecked Sendable {
    typealias Scheduler = (TimeInterval, @escaping @Sendable () -> Void) -> Void

    private let queue: DispatchQueue
    private let disk: DiskQueue
    private let endpoint: URL
    private let key: String
    private let session: URLSession
    private let backoff: Backoff
    private let clock: TrooperClock
    private let schedule: Scheduler
    var isOnline: () -> Bool = { true }

    private var sending = false
    private var failures = 0
    private var notBefore: TimeInterval = 0
    private var retryScheduled = false
    private var waiters: [() -> Void] = []

    /// Observability for tests.
    private(set) var lastDelay: TimeInterval?

    init(queue: DispatchQueue, disk: DiskQueue, endpoint: URL, key: String, session: URLSession,
         backoff: Backoff = Backoff(), clock: TrooperClock = .system, schedule: Scheduler? = nil) {
        self.queue = queue
        self.disk = disk
        self.endpoint = endpoint
        self.key = key
        self.session = session
        self.backoff = backoff
        self.clock = clock
        self.schedule = schedule ?? { delay, work in queue.asyncAfter(deadline: .now() + delay, execute: work) }
    }

    /// Sends queued batches until the queue is empty or a retryable failure occurs; `done` runs then.
    func drain(_ done: (() -> Void)? = nil) {
        if let done { waiters.append(done) }
        guard !sending else { return }
        guard clock.uptime() >= notBefore, isOnline(), let file = disk.oldest() else { return finish() }
        guard let body = try? Data(contentsOf: file), !body.isEmpty else {
            disk.remove(file)
            return drain()
        }
        var req = URLRequest(url: endpoint)
        req.httpMethod = "POST"
        req.timeoutInterval = 30
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.setValue("gzip", forHTTPHeaderField: "Content-Encoding")
        req.setValue(key, forHTTPHeaderField: "x-trooper-key")
        sending = true
        let task = session.uploadTask(with: req, from: body) { [self] _, response, error in
            let http = response as? HTTPURLResponse
            let status = http?.statusCode
            let retryAfter = http?.value(forHTTPHeaderField: "Retry-After")
            queue.async { self.handle(file: file, status: status, retryAfter: retryAfter, error: error) }
        }
        task.resume()
    }

    private func handle(file: URL, status: Int?, retryAfter: String?, error: Error?) {
        sending = false
        switch UploadOutcome.classify(statusCode: status, retryAfter: retryAfter, hasError: error != nil) {
        case .success:
            failures = 0
            disk.remove(file)
            drain()
        case .drop:
            TrooperLog.debug("batch rejected with HTTP \(status ?? 0), dropped")
            failures = 0
            disk.remove(file)
            drain()
        case .retry(let after):
            failures += 1
            let delay = after ?? backoff.delay(attempt: failures)
            lastDelay = delay
            notBefore = clock.uptime() + delay
            TrooperLog.debug("upload failed (\(status.map(String.init) ?? error.map { "\($0)" } ?? "?")), retry in \(delay)s")
            if !retryScheduled {
                retryScheduled = true
                schedule(delay) { [self] in
                    queue.async {
                        self.retryScheduled = false
                        self.drain()
                    }
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
