import XCTest
@testable import TaskTrooper

final class TransportTests: XCTestCase {
    private var queue: DispatchQueue!
    private var disk: DiskQueue!
    private var scheduled: Locked<[(TimeInterval, @Sendable () -> Void)]>!

    override func setUp() {
        queue = DispatchQueue(label: "test.uploader")
        disk = DiskQueue(directory: tempDirectory(), maxBytes: 5 * 1024 * 1024)
        scheduled = Locked([])
        StubProtocol.reset()
    }

    private func makeUploader(backoffRandom: Double = 1) -> Uploader {
        let s = scheduled!
        return Uploader(queue: queue, disk: disk, endpoint: URL(string: "https://ingest.test/v1/rum")!, key: "k1",
                        session: StubProtocol.session(), backoff: Backoff(random: { backoffRandom }),
                        schedule: { delay, work in s.withLock { $0.append((delay, work)) } })
    }

    private func drain(_ u: Uploader) {
        let exp = expectation(description: "drain")
        queue.async { u.drain { exp.fulfill() } }
        wait(for: [exp], timeout: 5)
    }

    private func enqueueBatch() {
        let batch = RumBatch(resource: RumResource(app: "a", version: "1", environment: "test", platform: .ios,
                                                   sdk: RumSdk(name: "x", version: "1")),
                             events: [RumEvent(type: .custom, tsMs: 1, sessionId: "s", anonymousId: "a")])
        let json = try! RumJSON.encoder().encode(batch)
        queue.sync { disk.enqueue(Gzip.compress(json)!, nowMs: 1) }
    }

    func testSuccessDeletesBatchAndSendsHeaders() throws {
        enqueueBatch()
        let u = makeUploader()
        drain(u)
        let req = try XCTUnwrap(StubProtocol.captured.current.first)
        XCTAssertEqual(req.request.httpMethod, "POST")
        XCTAssertEqual(req.request.value(forHTTPHeaderField: "x-trooper-key"), "k1")
        XCTAssertEqual(req.request.value(forHTTPHeaderField: "Content-Encoding"), "gzip")
        let json = try XCTUnwrap(Gzip.decompress(req.body))
        let decoded = try JSONDecoder().decode(RumBatch.self, from: json)
        XCTAssertEqual(decoded.events.count, 1)
        XCTAssertNil(disk.oldest())
    }

    func testServerErrorKeepsBatchAndBacksOff() {
        StubProtocol.reset { _ in (503, [:], Data(), nil) }
        enqueueBatch()
        let u = makeUploader(backoffRandom: 1)
        drain(u)
        XCTAssertNotNil(disk.oldest(), "batch kept for retry")
        XCTAssertEqual(scheduled.current.first?.0, 1, "first retry after 1 s")
        XCTAssertEqual(StubProtocol.captured.current.count, 1)

        // Retry succeeds.
        StubProtocol.reset()
        let exp = expectation(description: "retry")
        queue.async { u.drain { exp.fulfill() } }  // waiter, still inside backoff window → returns immediately
        wait(for: [exp], timeout: 5)
        XCTAssertEqual(StubProtocol.captured.current.count, 0, "no request during backoff window")
    }

    func testNetworkErrorRetries() {
        StubProtocol.reset { _ in (0, [:], Data(), URLError(.notConnectedToInternet)) }
        enqueueBatch()
        drain(makeUploader())
        XCTAssertNotNil(disk.oldest())
        XCTAssertEqual(scheduled.current.count, 1)
    }

    func testRetryAfterIsRespected() {
        StubProtocol.reset { _ in (429, ["Retry-After": "7"], Data(), nil) }
        enqueueBatch()
        let u = makeUploader()
        drain(u)
        XCTAssertEqual(scheduled.current.first?.0, 7)
        XCTAssertNotNil(disk.oldest())
    }

    func testClientErrorDropsBatch() {
        StubProtocol.reset { _ in (400, [:], Data(), nil) }
        enqueueBatch()
        drain(makeUploader())
        XCTAssertNil(disk.oldest())
        XCTAssertTrue(scheduled.current.isEmpty)
    }

    func testRetryAfterSuccessfulScheduleDrains() {
        let failing = Locked(true)
        StubProtocol.reset { _ in failing.current ? (500, [:], Data(), nil) : (200, [:], Data(), nil) }
        enqueueBatch()
        let clock = ManualClock()
        let s = scheduled!
        let u = Uploader(queue: queue, disk: disk, endpoint: URL(string: "https://ingest.test/v1/rum")!, key: "k",
                         session: StubProtocol.session(), backoff: Backoff(random: { 1 }), clock: clock.clock,
                         schedule: { delay, work in s.withLock { $0.append((delay, work)) } })
        drain(u)
        XCTAssertNotNil(disk.oldest())
        failing.set(false)
        clock.advance(seconds: 1)
        let pending = scheduled.current.first!.1
        pending()
        let disk = self.disk!
        let q = queue!
        for _ in 0..<300 where q.sync(execute: { disk.oldest() != nil }) { usleep(10_000) }
        XCTAssertNil(q.sync { disk.oldest() })
    }

    func testBackoffSchedule() {
        let max = Backoff(random: { 1 })
        XCTAssertEqual((1...8).map { max.delay(attempt: $0) }, [1, 2, 4, 8, 16, 32, 60, 60])
        let min = Backoff(random: { 0 })
        XCTAssertEqual(min.delay(attempt: 1), 0.5)
        XCTAssertEqual(min.delay(attempt: 20), 30)
    }

    func testClassification() {
        XCTAssertEqual(UploadOutcome.classify(statusCode: 200, retryAfter: nil, hasError: false), .success)
        XCTAssertEqual(UploadOutcome.classify(statusCode: 202, retryAfter: nil, hasError: false), .success)
        XCTAssertEqual(UploadOutcome.classify(statusCode: 400, retryAfter: nil, hasError: false), .drop)
        XCTAssertEqual(UploadOutcome.classify(statusCode: 401, retryAfter: nil, hasError: false), .drop)
        XCTAssertEqual(UploadOutcome.classify(statusCode: 413, retryAfter: nil, hasError: false), .drop)
        XCTAssertEqual(UploadOutcome.classify(statusCode: 429, retryAfter: "3", hasError: false), .retry(after: 3))
        XCTAssertEqual(UploadOutcome.classify(statusCode: 500, retryAfter: nil, hasError: false), .retry(after: nil))
        XCTAssertEqual(UploadOutcome.classify(statusCode: nil, retryAfter: nil, hasError: true), .retry(after: nil))
    }

    func testDiskQueueCapDropsOldest() {
        let q = DiskQueue(directory: tempDirectory(), maxBytes: 1000)!
        for i in 0..<5 { q.enqueue(Data(repeating: UInt8(i), count: 300), nowMs: Int64(1000 + i)) }
        let files = q.files()
        XCTAssertLessThanOrEqual(q.totalBytes, 1000)
        XCTAssertEqual(files.count, 3)
        XCTAssertEqual(try Data(contentsOf: files[0].url).first, 2, "two oldest dropped (FIFO)")
    }

    func testGzipRoundTrip() throws {
        let input = Data(String(repeating: "{\"type\":\"VIEW_START\"}", count: 200).utf8)
        let gz = try XCTUnwrap(Gzip.compress(input))
        XCTAssertEqual(Array(gz.prefix(2)), [0x1f, 0x8b], "gzip magic")
        XCTAssertLessThan(gz.count, input.count)
        XCTAssertEqual(Gzip.decompress(gz), input)
    }
}
