import Foundation

/// Removes credentials, fragment and query *values* (keys are kept) — PII is off by default.
enum URLSanitizer {
    static func sanitize(_ url: URL) -> String {
        guard var c = URLComponents(url: url, resolvingAgainstBaseURL: false) else {
            return url.absoluteString.components(separatedBy: "?").first ?? ""
        }
        c.user = nil
        c.password = nil
        c.fragment = nil
        if let items = c.queryItems, !items.isEmpty {
            c.queryItems = items.map { URLQueryItem(name: $0.name, value: nil) }
        } else {
            c.query = nil
        }
        return c.string ?? url.absoluteString.components(separatedBy: "?").first ?? ""
    }
}
