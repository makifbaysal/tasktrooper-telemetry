import Foundation
@testable import TaskTrooper

/// URLProtocol stub: records requests (with bodies) and answers from a configurable handler.
final class StubProtocol: URLProtocol {
    struct Captured {
        var request: URLRequest
        var body: Data
    }

    typealias Responder = @Sendable (URLRequest) -> (status: Int, headers: [String: String], body: Data, error: Error?)

    static let responder = Locked<Responder>({ _ in (200, [:], Data(), nil) })
    static let captured = Locked<[Captured]>([])

    static func reset(_ r: Responder? = nil) {
        captured.set([])
        responder.set(r ?? { _ in (200, [:], Data(), nil) })
    }

    static func session() -> URLSession {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [StubProtocol.self]
        return URLSession(configuration: config)
    }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        var body = request.httpBody ?? Data()
        if body.isEmpty, let stream = request.httpBodyStream {
            stream.open()
            var buf = [UInt8](repeating: 0, count: 64 * 1024)
            while stream.hasBytesAvailable {
                let n = stream.read(&buf, maxLength: buf.count)
                if n <= 0 { break }
                body.append(buf, count: n)
            }
            stream.close()
        }
        Self.captured.withLock { $0.append(Captured(request: request, body: body)) }
        let r = Self.responder.current(request)
        if let error = r.error {
            client?.urlProtocol(self, didFailWithError: error)
            return
        }
        let response = HTTPURLResponse(url: request.url!, statusCode: r.status, httpVersion: "HTTP/1.1", headerFields: r.headers)!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: r.body)
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}

func tempDirectory() -> URL {
    let url = FileManager.default.temporaryDirectory.appendingPathComponent("tt-tests-\(UUID().uuidString)", isDirectory: true)
    try? FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
    return url
}

func freshDefaults() -> UserDefaults {
    let name = "tt-tests-\(UUID().uuidString)"
    let d = UserDefaults(suiteName: name)!
    d.removePersistentDomain(forName: name)
    return d
}

final class ManualClock: @unchecked Sendable {
    let ms = Locked<Int64>(1_700_000_000_000)
    let up = Locked<TimeInterval>(1000)

    var clock: TrooperClock {
        TrooperClock(nowMs: { [ms] in ms.current }, uptime: { [up] in up.current })
    }

    func advance(seconds: Double) {
        ms.withLock { $0 += Int64(seconds * 1000) }
        up.withLock { $0 += seconds }
    }
}
