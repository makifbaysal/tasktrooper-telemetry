import XCTest
@_spi(TrooperReplay) import TaskTrooper
@testable import TaskTrooperReplay

final class ReplayStubProtocol: URLProtocol {
    static let statuses = ReplayLock<[Int]>([])
    static let requests = ReplayLock<[URLRequest]>([])

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        Self.requests.withLock { $0.append(request) }
        let status = Self.statuses.withLock { $0.isEmpty ? 200 : $0.removeFirst() }
        let resp = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: nil, headerFields: [:])!
        client?.urlProtocol(self, didReceive: resp, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data())
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}

    static func session() -> URLSession {
        let c = URLSessionConfiguration.ephemeral
        c.protocolClasses = [ReplayStubProtocol.self]
        return URLSession(configuration: c)
    }
}

final class SegmentUploaderTests: XCTestCase {
    func testUploadsWithHeadersRetriesAndDrops() throws {
        ReplayStubProtocol.statuses.set([503, 200, 400])
        ReplayStubProtocol.requests.set([])
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("tt-replay-up-\(UUID().uuidString)")
        let store = try XCTUnwrap(SegmentStore(directory: dir, maxBytes: 1_000_000))
        store.write(Data([1]), nowMs: 1)
        store.write(Data([2]), nowMs: 2)
        let queue = DispatchQueue(label: "test.replay")
        let scheduled = ReplayLock<[@Sendable () -> Void]>([])
        let up = SegmentUploader(queue: queue, store: store, session: ReplayStubProtocol.session(),
                                 endpoint: { (URL(string: "https://ingest.test/v1/replay")!, "key1") },
                                 schedule: { _, work in scheduled.withLock { $0.append(work) } })

        let first = expectation(description: "first drain")
        queue.async { up.drain { first.fulfill() } }
        wait(for: [first], timeout: 5)
        XCTAssertEqual(store.files().count, 2, "503 keeps the segment")
        XCTAssertNotNil(up.lastDelay)

        let second = expectation(description: "retry drain")
        queue.async {
            scheduled.current.forEach { $0() }
            queue.async { up.drain { second.fulfill() } }
        }
        wait(for: [second], timeout: 5)
        let done = expectation(description: "settle")
        func poll() { queue.asyncAfter(deadline: .now() + 0.05) { store.files().isEmpty ? done.fulfill() : poll() } }
        poll()
        wait(for: [done], timeout: 5)

        let reqs = ReplayStubProtocol.requests.current
        XCTAssertEqual(reqs.count, 3)
        XCTAssertEqual(reqs[0].url?.absoluteString, "https://ingest.test/v1/replay")
        XCTAssertEqual(reqs[0].httpMethod, "POST")
        XCTAssertEqual(reqs[0].value(forHTTPHeaderField: "Content-Encoding"), "gzip")
        XCTAssertEqual(reqs[0].value(forHTTPHeaderField: "x-trooper-key"), "key1")
    }
}
