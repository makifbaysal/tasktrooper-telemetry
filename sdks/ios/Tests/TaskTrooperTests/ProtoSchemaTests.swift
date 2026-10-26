import XCTest
@testable import TaskTrooper

/// Verifies the JSON produced by the SDK uses exactly the field names / enum names of rum.proto.
final class ProtoSchemaTests: XCTestCase {
    struct Proto {
        var messages: [String: Set<String>] = [:]
        var enums: [String: Set<String>] = [:]
    }

    static func protoURL() -> URL {
        URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent("proto/rum/v1/rum.proto")
    }

    static func parse(_ text: String) -> Proto {
        var p = Proto()
        var currentMessage: String?
        var currentEnum: String?
        let field = try! NSRegularExpression(pattern: #"^\s*(?:repeated\s+)?(?:map<[^>]+>|[\w.]+)\s+(\w+)\s*=\s*\d+\s*;"#)
        let enumValue = try! NSRegularExpression(pattern: #"^\s*([A-Z0-9_]+)\s*=\s*\d+\s*;"#)
        for raw in text.components(separatedBy: "\n") {
            let line = raw.components(separatedBy: "//")[0]
            if line.hasPrefix("message "), let name = line.split(separator: " ").dropFirst().first {
                currentMessage = String(name)
                p.messages[String(name)] = []
                continue
            }
            if line.hasPrefix("enum "), let name = line.split(separator: " ").dropFirst().first {
                currentEnum = String(name)
                p.enums[String(name)] = []
                continue
            }
            if line.hasPrefix("}") {
                currentMessage = nil
                currentEnum = nil
                continue
            }
            let range = NSRange(line.startIndex..., in: line)
            if let m = currentMessage, let match = field.firstMatch(in: line, range: range), let r = Range(match.range(at: 1), in: line) {
                p.messages[m]?.insert(String(line[r]))
            } else if let e = currentEnum, let match = enumValue.firstMatch(in: line, range: range), let r = Range(match.range(at: 1), in: line) {
                p.enums[e]?.insert(String(line[r]))
            }
        }
        return p
    }

    private func loadProto() throws -> Proto {
        let url = Self.protoURL()
        guard let text = try? String(contentsOf: url, encoding: .utf8) else {
            throw XCTSkip("rum.proto not found at \(url.path)")
        }
        let p = Self.parse(text)
        XCTAssertGreaterThan(p.messages.count, 10, "proto parsed")
        return p
    }

    /// JSON object key → proto message type of that key's value (nil = map / scalar).
    static let nested: [String: [String: String]] = [
        "RumBatch": ["resource": "Resource", "events": "RumEvent"],
        "Resource": ["sdk": "Sdk", "device": "Device", "os": "Os", "browser": "Browser"],
        "RumEvent": ["user": "User", "view": "ViewPayload", "vital": "VitalPayload", "error": "ErrorPayload",
                     "resource": "ResourcePayload", "action": "ActionPayload", "app_start": "AppStartPayload",
                     "custom": "CustomPayload"],
    ]

    private func collect(_ json: Any, as message: String, into seen: inout [String: Set<String>]) {
        if let array = json as? [Any] {
            array.forEach { collect($0, as: message, into: &seen) }
            return
        }
        guard let obj = json as? [String: Any] else { return }
        seen[message, default: []].formUnion(obj.keys)
        for (key, value) in obj {
            if let child = Self.nested[message]?[key] { collect(value, as: child, into: &seen) }
        }
    }

    private func fullBatch() -> RumBatch {
        let resource = RumResource(
            app: "shop-ios", version: "1.0", environment: "test", platform: .ios,
            sdk: RumSdk(name: "TaskTrooper-iOS", version: "0.1.0"),
            device: RumDevice(type: "mobile", manufacturer: "Apple", model: "iPhone16,1", locale: "en_US", screenWidth: 1179, screenHeight: 2556),
            os: RumOs(name: "iOS", version: "18.0"), attributes: ["k": "v"])
        func ev(_ t: RumEventType) -> RumEvent {
            RumEvent(type: t, tsMs: 1, sessionId: "s", viewId: "v", viewName: "Home", anonymousId: "a",
                     user: RumUser(id: "u", traits: ["plan": "pro"]), connection: "wifi", attributes: ["x": "y"])
        }
        var view = ev(.viewEnd)
        view.view = RumViewPayload(url: "", referrer: "", loadingTimeMs: 1, timeSpentMs: 2, isInitial: true)
        var vital = ev(.vital)
        vital.vital = RumVitalPayload(name: "SLOW_FRAMES", value: 1, rating: "good")
        var error = ev(.error)
        error.error = RumErrorPayload(type: "SIGSEGV", message: "m", stack: "s", source: "crash", isCrash: true, thread: "main", fingerprint: "f")
        var res = ev(.resource)
        res.resource = RumResourcePayload(url: "https://x", method: "GET", statusCode: 200, durationMs: 1, requestBytes: 1,
                                          responseBytes: 2, traceId: "t", spanId: "s", initiator: "urlsession")
        var action = ev(.action)
        action.action = RumActionPayload(name: "Buy", target: "btn", rage: false, dead: false)
        var start = ev(.appStart)
        start.appStart = RumAppStartPayload(kind: "cold", durationMs: 900)
        var custom = ev(.custom)
        custom.custom = RumCustomPayload(name: "checkout", props: ["a": "b"], value: 3)
        return RumBatch(resource: resource, events: [ev(.sessionStart), view, vital, error, res, action, start, custom])
    }

    func testJSONFieldNamesMatchProto() throws {
        let proto = try loadProto()
        let data = try RumJSON.encoder().encode(fullBatch())
        let json = try JSONSerialization.jsonObject(with: data)
        var seen: [String: Set<String>] = [:]
        collect(json, as: "RumBatch", into: &seen)
        XCTAssertGreaterThanOrEqual(seen.count, 12)
        for (message, keys) in seen {
            let fields = try XCTUnwrap(proto.messages[message], "message \(message) missing in proto")
            XCTAssertTrue(keys.isSubset(of: fields), "\(message): unknown JSON keys \(keys.subtracting(fields))")
            // Every proto field is representable (except web-only browser info).
            let missing = fields.subtracting(keys).subtracting(["browser"])
            XCTAssertTrue(missing.isEmpty, "\(message): proto fields not produced by SDK model: \(missing)")
        }
    }

    func testEnumNamesMatchProto() throws {
        let proto = try loadProto()
        let eventTypes = try XCTUnwrap(proto.enums["EventType"])
        let platforms = try XCTUnwrap(proto.enums["Platform"])
        let sdkTypes: [RumEventType] = [.unspecified, .sessionStart, .viewStart, .viewEnd, .vital, .error, .resource, .action, .appStart, .custom]
        XCTAssertEqual(Set(sdkTypes.map(\.rawValue)), eventTypes)
        let sdkPlatforms: [RumPlatform] = [.unspecified, .web, .android, .ios, .flutter, .reactNative]
        XCTAssertEqual(Set(sdkPlatforms.map(\.rawValue)), platforms)

        let data = try RumJSON.encoder().encode(fullBatch())
        let obj = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
        XCTAssertEqual((obj["resource"] as? [String: Any])?["platform"] as? String, "PLATFORM_IOS")
        let first = try XCTUnwrap((obj["events"] as? [[String: Any]])?.first)
        XCTAssertEqual(first["type"] as? String, "SESSION_START")
        XCTAssertEqual(first["ts_ms"] as? Int, 1, "int64 encoded as JSON number")
    }

    func testNilFieldsAreOmitted() throws {
        let e = RumEvent(type: .custom, tsMs: 5, sessionId: "s", anonymousId: "a")
        let obj = try XCTUnwrap(JSONSerialization.jsonObject(with: RumJSON.encoder().encode(e)) as? [String: Any])
        XCTAssertEqual(Set(obj.keys), ["type", "ts_ms", "session_id", "anonymous_id"])
    }
}
