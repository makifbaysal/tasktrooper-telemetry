import Foundation

// JSON mapping of proto/rum/v1/rum.proto (protojson with UseProtoNames: snake_case field names,
// enum values as their names). Nil optionals are omitted.

public enum RumPlatform: String, Codable, Sendable {
    case unspecified = "PLATFORM_UNSPECIFIED"
    case web = "PLATFORM_WEB"
    case android = "PLATFORM_ANDROID"
    case ios = "PLATFORM_IOS"
    case flutter = "PLATFORM_FLUTTER"
    case reactNative = "PLATFORM_REACT_NATIVE"
}

public enum RumEventType: String, Codable, Sendable {
    case unspecified = "EVENT_TYPE_UNSPECIFIED"
    case sessionStart = "SESSION_START"
    case viewStart = "VIEW_START"
    case viewEnd = "VIEW_END"
    case vital = "VITAL"
    case error = "ERROR"
    case resource = "RESOURCE"
    case action = "ACTION"
    case appStart = "APP_START"
    case custom = "CUSTOM"
}

public struct RumBatch: Codable, Sendable {
    public var resource: RumResource
    public var events: [RumEvent]
}

public struct RumResource: Codable, Sendable {
    public var app: String
    public var version: String
    public var environment: String
    public var platform: RumPlatform
    public var sdk: RumSdk
    public var device: RumDevice?
    public var os: RumOs?
    public var attributes: [String: String]?
}

public struct RumSdk: Codable, Sendable {
    public var name: String
    public var version: String
}

public struct RumDevice: Codable, Sendable {
    public var type: String?
    public var manufacturer: String?
    public var model: String?
    public var locale: String?
    public var screenWidth: Int32?
    public var screenHeight: Int32?

    enum CodingKeys: String, CodingKey {
        case type, manufacturer, model, locale
        case screenWidth = "screen_width"
        case screenHeight = "screen_height"
    }
}

public struct RumOs: Codable, Sendable {
    public var name: String
    public var version: String
}

public struct RumUser: Codable, Sendable, Equatable {
    public var id: String
    public var traits: [String: String]?

    public init(id: String, traits: [String: String]? = nil) {
        self.id = id
        self.traits = traits
    }
}

public struct RumEvent: Codable, Sendable {
    public var type: RumEventType
    public var tsMs: Int64
    public var sessionId: String
    public var viewId: String?
    public var viewName: String?
    public var anonymousId: String
    public var user: RumUser?
    public var connection: String?

    public var view: RumViewPayload?
    public var vital: RumVitalPayload?
    public var error: RumErrorPayload?
    public var resource: RumResourcePayload?
    public var action: RumActionPayload?
    public var appStart: RumAppStartPayload?
    public var custom: RumCustomPayload?

    public var attributes: [String: String]?

    enum CodingKeys: String, CodingKey {
        case type
        case tsMs = "ts_ms"
        case sessionId = "session_id"
        case viewId = "view_id"
        case viewName = "view_name"
        case anonymousId = "anonymous_id"
        case user, connection, view, vital, error, resource, action
        case appStart = "app_start"
        case custom, attributes
    }
}

public struct RumViewPayload: Codable, Sendable {
    public var url: String?
    public var referrer: String?
    public var loadingTimeMs: Double?
    public var timeSpentMs: Double?
    public var isInitial: Bool?

    enum CodingKeys: String, CodingKey {
        case url, referrer
        case loadingTimeMs = "loading_time_ms"
        case timeSpentMs = "time_spent_ms"
        case isInitial = "is_initial"
    }
}

public struct RumVitalPayload: Codable, Sendable {
    public var name: String
    public var value: Double
    public var rating: String?
}

public struct RumErrorPayload: Codable, Sendable {
    public var type: String
    public var message: String
    public var stack: String?
    public var source: String
    public var isCrash: Bool?
    public var thread: String?
    public var fingerprint: String?

    enum CodingKeys: String, CodingKey {
        case type, message, stack, source
        case isCrash = "is_crash"
        case thread, fingerprint
    }
}

public struct RumResourcePayload: Codable, Sendable {
    public var url: String
    public var method: String
    public var statusCode: Int32
    public var durationMs: Double
    public var requestBytes: Int64?
    public var responseBytes: Int64?
    public var traceId: String?
    public var spanId: String?
    public var initiator: String?

    enum CodingKeys: String, CodingKey {
        case url, method
        case statusCode = "status_code"
        case durationMs = "duration_ms"
        case requestBytes = "request_bytes"
        case responseBytes = "response_bytes"
        case traceId = "trace_id"
        case spanId = "span_id"
        case initiator
    }
}

public struct RumActionPayload: Codable, Sendable {
    public var name: String
    public var target: String?
    public var rage: Bool?
    public var dead: Bool?
}

public struct RumAppStartPayload: Codable, Sendable {
    public var kind: String
    public var durationMs: Double

    enum CodingKeys: String, CodingKey {
        case kind
        case durationMs = "duration_ms"
    }
}

public struct RumCustomPayload: Codable, Sendable {
    public var name: String
    public var props: [String: String]?
    public var value: Double?
}

enum RumJSON {
    static func encoder() -> JSONEncoder {
        let e = JSONEncoder()
        e.outputFormatting = [.withoutEscapingSlashes]
        return e
    }
}
