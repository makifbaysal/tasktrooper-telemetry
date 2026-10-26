import Foundation

/// Decides which requests get a W3C `traceparent` (plus `baggage: tt.svc=<app>`) and builds the headers.
struct TracePropagation: Sendable {
    private let hosts: [String]
    private let wildcard: Bool
    private let patterns: [String]

    init(targets: [String]) {
        var hosts: [String] = []
        var patterns: [String] = []
        var wildcard = false
        for raw in targets {
            let t = raw.trimmingCharacters(in: .whitespaces)
            if t.isEmpty { continue }
            if t == "*" { wildcard = true }
            else if t.hasPrefix("^") { patterns.append(t) }
            else {
                var h = t.lowercased()
                if h.hasPrefix("*.") { h.removeFirst(2) }
                if h.hasPrefix(".") { h.removeFirst() }
                hosts.append(h)
            }
        }
        self.hosts = hosts
        self.patterns = patterns
        self.wildcard = wildcard
    }

    var isEmpty: Bool { hosts.isEmpty && patterns.isEmpty && !wildcard }

    func matches(_ url: URL) -> Bool {
        if wildcard { return true }
        if let host = url.host?.lowercased(),
           hosts.contains(where: { host == $0 || host.hasSuffix("." + $0) }) {
            return true
        }
        let s = url.absoluteString
        return patterns.contains { p in
            s.range(of: p, options: .regularExpression) != nil
        }
    }

    /// `00-<32 hex trace-id>-<16 hex parent-id>-<flags>`
    static func header(traceId: String, spanId: String, sampled: Bool = true) -> String {
        "00-\(traceId)-\(spanId)-\(sampled ? "01" : "00")"
    }

    /// W3C baggage key carrying the calling app name (backends read it into `trooper.caller`).
    static let baggageServiceKey = "tt.svc"
    /// W3C limit for the whole `baggage` header.
    static let baggageMaxBytes = 8192

    private static let baggageValueAllowed: CharacterSet = {
        var s = CharacterSet()
        s.insert(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~")
        return s
    }()

    /// Merges `tt.svc=<service>` (percent-encoded) into an existing `baggage` value, replacing any existing
    /// `tt.svc` member. Returns `existing` unchanged when `service` is empty or the result would exceed
    /// `baggageMaxBytes`.
    static func mergeBaggage(_ existing: String?, service: String?) -> String? {
        guard let service, !service.isEmpty,
              let encoded = service.addingPercentEncoding(withAllowedCharacters: baggageValueAllowed)
        else { return existing }
        let others = (existing ?? "").split(separator: ",").compactMap { raw -> String? in
            let m = raw.trimmingCharacters(in: .whitespaces)
            if m.isEmpty { return nil }
            let key = m.split(whereSeparator: { $0 == "=" || $0 == ";" }).first.map {
                $0.trimmingCharacters(in: .whitespaces)
            } ?? ""
            return key == baggageServiceKey ? nil : m
        }
        let out = (["\(baggageServiceKey)=\(encoded)"] + others).joined(separator: ",")
        return out.utf8.count <= baggageMaxBytes ? out : existing
    }

    static func isValid(_ header: String) -> Bool {
        header.range(of: "^00-[0-9a-f]{32}-[0-9a-f]{16}-0[01]$", options: .regularExpression) != nil
            && !header.contains("-00000000000000000000000000000000-")
            && !header.contains("-0000000000000000-")
    }
}
