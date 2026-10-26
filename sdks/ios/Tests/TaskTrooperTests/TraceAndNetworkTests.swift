import XCTest
@testable import TaskTrooper

final class TraceparentTests: XCTestCase {
    func testHeaderFormat() {
        for _ in 0..<50 {
            let h = TracePropagation.header(traceId: IDs.traceId(), spanId: IDs.spanId())
            XCTAssertTrue(TracePropagation.isValid(h), h)
            XCTAssertEqual(h.count, 55)
        }
        XCTAssertEqual(TracePropagation.header(traceId: String(repeating: "a", count: 32), spanId: String(repeating: "b", count: 16)),
                       "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01")
        XCTAssertFalse(TracePropagation.isValid("00-00000000000000000000000000000000-bbbbbbbbbbbbbbbb-01"))
        XCTAssertFalse(TracePropagation.isValid("01-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01"))
    }

    func testTargetMatching() {
        let p = TracePropagation(targets: ["api.shop.com", "*.internal.dev", "^https://pay\\.example\\.com/v[0-9]+/"])
        XCTAssertTrue(p.matches(URL(string: "https://api.shop.com/orders")!))
        XCTAssertTrue(p.matches(URL(string: "https://eu.api.shop.com/orders")!))
        XCTAssertFalse(p.matches(URL(string: "https://evilapi.shop.com/")!))
        XCTAssertFalse(p.matches(URL(string: "https://api.shop.com.evil.io/")!))
        XCTAssertTrue(p.matches(URL(string: "http://svc.internal.dev:8080/x")!))
        XCTAssertTrue(p.matches(URL(string: "https://pay.example.com/v2/charge")!))
        XCTAssertFalse(p.matches(URL(string: "https://pay.example.com/health")!))
        XCTAssertTrue(TracePropagation(targets: ["*"]).matches(URL(string: "https://anything.io")!))
        XCTAssertTrue(TracePropagation(targets: []).isEmpty)
    }

    func testURLSanitizerStripsQueryValuesAndCredentials() {
        XCTAssertEqual(URLSanitizer.sanitize(URL(string: "https://u:p@api.shop.com/search?q=shoes&page=2#top")!),
                       "https://api.shop.com/search?q&page")
        XCTAssertEqual(URLSanitizer.sanitize(URL(string: "https://api.shop.com/a/b")!), "https://api.shop.com/a/b")
    }
}

final class NetworkInstrumentationTests: XCTestCase {
    private let records = Locked<[NetworkInstrumentation.Record]>([])

    override func setUp() {
        StubProtocol.reset { req in
            req.url?.path == "/fail" ? (500, [:], Data("x".utf8), nil) : (200, [:], Data("hello".utf8), nil)
        }
        records.set([])
        let records = self.records
        NetworkInstrumentation.shared.enable(.init(
            propagation: TracePropagation(targets: ["api.example.test"]),
            shouldPropagate: { true },
            onComplete: { r in records.withLock { $0.append(r) } },
            serviceName: "shop ios"))
    }

    override func tearDown() {
        NetworkInstrumentation.shared.disable()
    }

    private func waitForRecords(_ n: Int) {
        let exp = expectation(description: "records")
        let records = self.records
        DispatchQueue.global().async {
            for _ in 0..<200 where records.current.count < n { usleep(10_000) }
            exp.fulfill()
        }
        wait(for: [exp], timeout: 5)
    }

    func testInjectsTraceparentAndRecordsResource() throws {
        let session = StubProtocol.session()
        let done = expectation(description: "request")
        session.dataTask(with: URL(string: "https://api.example.test/orders?id=42")!) { _, _, _ in done.fulfill() }.resume()
        wait(for: [done], timeout: 5)
        waitForRecords(1)

        let sent = try XCTUnwrap(StubProtocol.captured.current.first)
        let header = try XCTUnwrap(sent.request.value(forHTTPHeaderField: "traceparent"), "traceparent injected")
        XCTAssertTrue(TracePropagation.isValid(header))
        XCTAssertEqual(sent.request.value(forHTTPHeaderField: "baggage"), "tt.svc=shop%20ios")

        let r = try XCTUnwrap(records.current.first)
        XCTAssertEqual(r.statusCode, 200)
        XCTAssertEqual(r.method, "GET")
        XCTAssertEqual(r.responseBytes, 5)
        XCTAssertGreaterThanOrEqual(r.durationMs, 0)
        XCTAssertEqual(header, "00-\(r.traceId!)-\(r.spanId!)-01")
    }

    func testMergesExistingBaggage() throws {
        let session = StubProtocol.session()
        let done = expectation(description: "request")
        var req = URLRequest(url: URL(string: "https://api.example.test/x")!)
        req.setValue("k=v, tt.svc=old;p=1", forHTTPHeaderField: "baggage")
        session.dataTask(with: req) { _, _, _ in done.fulfill() }.resume()
        wait(for: [done], timeout: 5)
        waitForRecords(1)
        let sent = try XCTUnwrap(StubProtocol.captured.current.first)
        XCTAssertEqual(sent.request.value(forHTTPHeaderField: "baggage"), "tt.svc=shop%20ios,k=v")
        XCTAssertNotNil(sent.request.value(forHTTPHeaderField: "traceparent"))
    }

    func testBaggageMergeRules() {
        XCTAssertEqual(TracePropagation.mergeBaggage(nil, service: "a,b=c"), "tt.svc=a%2Cb%3Dc")
        XCTAssertEqual(TracePropagation.mergeBaggage("tt.svc=x, a=1 ,, b=2;prop", service: "svc"), "tt.svc=svc,a=1,b=2;prop")
        XCTAssertEqual(TracePropagation.mergeBaggage("a=1", service: ""), "a=1")
        let big = "k=" + String(repeating: "x", count: TracePropagation.baggageMaxBytes - 5)
        XCTAssertEqual(TracePropagation.mergeBaggage(big, service: "svc"), big)
    }

    func testNoTraceparentForOtherHosts() throws {
        let session = StubProtocol.session()
        let done = expectation(description: "request")
        var req = URLRequest(url: URL(string: "https://cdn.other.test/fail")!)
        req.httpMethod = "POST"
        req.httpBody = Data("abc".utf8)
        session.dataTask(with: req) { _, _, _ in done.fulfill() }.resume()
        wait(for: [done], timeout: 5)
        waitForRecords(1)
        XCTAssertNil(StubProtocol.captured.current.first?.request.value(forHTTPHeaderField: "traceparent"))
        XCTAssertNil(StubProtocol.captured.current.first?.request.value(forHTTPHeaderField: "baggage"))
        let r = try XCTUnwrap(records.current.first)
        XCTAssertEqual(r.statusCode, 500)
        XCTAssertEqual(r.method, "POST")
        XCTAssertNil(r.traceId)
    }

    func testSkipsSDKOwnRequests() {
        let session = StubProtocol.session()
        let done = expectation(description: "request")
        var req = URLRequest(url: URL(string: "https://api.example.test/v1/rum")!)
        req.setValue("key", forHTTPHeaderField: "x-trooper-key")
        session.dataTask(with: req) { _, _, _ in done.fulfill() }.resume()
        wait(for: [done], timeout: 5)
        usleep(100_000)
        XCTAssertTrue(records.current.isEmpty)
        XCTAssertNil(StubProtocol.captured.current.first?.request.value(forHTTPHeaderField: "traceparent"))
    }

    func testAsyncAwaitRequestsAreTracked() async throws {
        guard #available(macOS 12.0, iOS 15.0, tvOS 15.0, *) else { throw XCTSkip("async URLSession API unavailable") }
        let session = StubProtocol.session()
        _ = try await session.data(from: URL(string: "https://api.example.test/async")!)
        for _ in 0..<200 where records.current.isEmpty { try await Task.sleep(nanoseconds: 10_000_000) }
        XCTAssertEqual(records.current.count, 1)
        XCTAssertNotNil(StubProtocol.captured.current.first?.request.value(forHTTPHeaderField: "traceparent"))
    }
}
