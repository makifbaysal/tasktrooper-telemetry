import Foundation

/// Exponential backoff 1 s → 60 s with jitter (contracts.md §2). Delay for attempt n (1-based) is
/// d = min(max, base·2^(n-1)), jittered into [d/2, d].
struct Backoff: Sendable {
    var base: TimeInterval = 1
    var max: TimeInterval = 60
    var random: @Sendable () -> Double = { Double.random(in: 0...1) }

    func delay(attempt: Int) -> TimeInterval {
        let n = Swift.max(1, attempt)
        let exp = base * pow(2, Double(Swift.min(n - 1, 30)))
        let d = Swift.min(max, exp)
        return d / 2 + (d / 2) * random()
    }
}

enum UploadOutcome: Equatable {
    case success
    /// Permanent failure (4xx except 429): drop the batch.
    case drop
    /// 429 / 5xx / network error: keep and retry later (optional server-provided delay).
    case retry(after: TimeInterval?)

    static func classify(statusCode: Int?, retryAfter: String?, hasError: Bool) -> UploadOutcome {
        guard let code = statusCode, !hasError || code > 0 else { return .retry(after: nil) }
        switch code {
        case 200..<300: return .success
        case 429: return .retry(after: parseRetryAfter(retryAfter))
        case 500..<600: return .retry(after: parseRetryAfter(retryAfter))
        default: return .drop
        }
    }

    /// `Retry-After` as delta-seconds or HTTP-date, capped at 10 minutes.
    static func parseRetryAfter(_ value: String?, now: Date = Date()) -> TimeInterval? {
        guard let v = value?.trimmingCharacters(in: .whitespaces), !v.isEmpty else { return nil }
        if let s = Double(v) { return Swift.min(Swift.max(0, s), 600) }
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.timeZone = TimeZone(identifier: "GMT")
        f.dateFormat = "EEE, dd MMM yyyy HH:mm:ss zzz"
        guard let date = f.date(from: v) else { return nil }
        return Swift.min(Swift.max(0, date.timeIntervalSince(now)), 600)
    }
}
