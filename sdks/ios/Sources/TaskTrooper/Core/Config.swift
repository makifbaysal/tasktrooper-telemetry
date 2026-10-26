import Foundation

/// Parsed DSN: `https://<ingest-key>@<host>[:port]/<project_id>` (see .ai/contracts.md §2).
public struct TrooperDSN: Sendable, Equatable {
    public let key: String
    /// Base URL (scheme, host, port and any path prefix before the project id).
    public let baseURL: URL
    public let projectId: String

    /// `POST /v1/rum` endpoint.
    public var rumURL: URL { baseURL.appendingPathComponent("v1/rum") }

    public init?(_ string: String) {
        guard let c = URLComponents(string: string.trimmingCharacters(in: .whitespacesAndNewlines)),
              let scheme = c.scheme?.lowercased(), scheme == "https" || scheme == "http",
              let host = c.host, !host.isEmpty,
              let user = c.user, !user.isEmpty
        else { return nil }
        let parts = c.path.split(separator: "/").map(String.init)
        guard let project = parts.last, !project.isEmpty else { return nil }
        var base = URLComponents()
        base.scheme = scheme
        base.host = host
        base.port = c.port
        base.path = parts.dropLast().isEmpty ? "" : "/" + parts.dropLast().joined(separator: "/")
        guard let url = base.url else { return nil }
        self.key = user.removingPercentEncoding ?? user
        self.baseURL = url
        self.projectId = project
    }
}

public struct TrooperConfig: Sendable {
    /// `https://<key>@<host>/<project_id>`
    public var dsn: String
    /// Client app name (the "service" shown in the dashboard), e.g. "shop-ios".
    public var app: String
    /// App version / build; used as deploy marker.
    public var version: String
    public var environment: String
    /// Fraction of sessions recorded (0...1), decided once per session.
    public var sampleRate: Double
    /// Hosts that receive W3C `traceparent` and `baggage: tt.svc=<app>` headers. `"api.shop.com"` matches the host and its
    /// subdomains, `"*"` matches everything, a value starting with `^` is a regex over the full URL.
    public var tracePropagationTargets: [String]

    /// UIKit `viewDidAppear`/`viewDidDisappear` swizzling.
    public var trackViews = true
    /// URLSession instrumentation (RESOURCE events + traceparent).
    public var trackNetwork = true
    /// Signal + NSException crash reporting.
    public var trackCrashes = true
    /// Main-thread watchdog.
    public var trackHangs = true
    public var hangThreshold: TimeInterval = 2
    /// Slow/frozen frames per view (CADisplayLink).
    public var trackFrames = true
    public var trackAppStart = true
    /// Forward MetricKit crash/hang diagnostics (iOS 14+). Off by default: overlaps with built-in crash reports.
    public var metricKit = false
    /// View controller class names never tracked (in addition to containers and system classes).
    public var ignoredViewControllers: Set<String> = []

    public var flushInterval: TimeInterval = 5
    public var maxBatchSize = 100
    public var maxQueueBytes = 5 * 1024 * 1024

    /// Last chance to mask or drop an event (return nil to drop).
    public var beforeSend: (@Sendable (RumEvent) -> RumEvent?)?
    /// Extra key/values sent on the batch resource.
    public var resourceAttributes: [String: String] = [:]
    /// Log SDK internals with NSLog.
    public var debug = false

    /// `"<CFBundleShortVersionString> (<CFBundleVersion>)"` of the main bundle, e.g. `"1.4.0 (812)"`.
    /// Pass it as `version` so RUM `resource.version` matches the release that `Scripts/upload-dsyms.sh`
    /// uses by default for dSYMs (symbolication looks dSYMs up by app + release).
    public static var bundleVersion: String { bundleVersion(info: Bundle.main.infoDictionary ?? [:]) }

    static func bundleVersion(info: [String: Any]) -> String {
        let short = (info["CFBundleShortVersionString"] as? String) ?? ""
        let build = (info["CFBundleVersion"] as? String) ?? ""
        switch (short.isEmpty, build.isEmpty) {
        case (false, false): return "\(short) (\(build))"
        case (false, true): return short
        case (true, false): return build
        default: return ""
        }
    }

    public init(
        dsn: String,
        app: String,
        version: String = TrooperConfig.bundleVersion,
        environment: String = "production",
        sampleRate: Double = 1.0,
        tracePropagationTargets: [String] = []
    ) {
        self.dsn = dsn
        self.app = app
        self.version = version
        self.environment = environment
        self.sampleRate = sampleRate
        self.tracePropagationTargets = tracePropagationTargets
    }
}
