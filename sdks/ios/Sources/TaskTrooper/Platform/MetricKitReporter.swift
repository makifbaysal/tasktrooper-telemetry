#if canImport(MetricKit) && os(iOS)
import Foundation
import MetricKit

/// Forwards MetricKit crash and hang diagnostics (iOS 14+) as ERROR events. Payloads arrive up to a
/// day later and are attributed to the current session; `metrickit=true` lets the server dedupe them
/// against the SDK's own crash reports.
@available(iOS 14.0, *)
final class MetricKitReporter: NSObject, MXMetricManagerSubscriber, @unchecked Sendable {
    @MainActor private static var instance: MetricKitReporter?
    private let core: TrooperCore

    private init(core: TrooperCore) {
        self.core = core
    }

    @MainActor
    static func install(core: TrooperCore) {
        guard instance == nil else { return }
        let r = MetricKitReporter(core: core)
        instance = r
        MXMetricManager.shared.add(r)
    }

    func didReceive(_ payloads: [MXDiagnosticPayload]) {
        for payload in payloads {
            let ts = Int64(payload.timeStampEnd.timeIntervalSince1970 * 1000)
            for crash in payload.crashDiagnostics ?? [] {
                let signal = crash.signal.map { "signal \($0)" } ?? ""
                let exc = crash.exceptionType.map { "exception \($0)" } ?? ""
                let reason = crash.terminationReason ?? ([exc, signal].filter { !$0.isEmpty }.joined(separator: ", "))
                emit(ts: ts, type: "MXCrashDiagnostic", message: reason.isEmpty ? "Crash" : reason,
                     stack: String(decoding: crash.callStackTree.jsonRepresentation(), as: UTF8.self),
                     source: "crash", isCrash: true, extra: ["app.build": crash.metaData.applicationBuildVersion])
            }
            for hang in payload.hangDiagnostics ?? [] {
                let ms = hang.hangDuration.converted(to: .milliseconds).value
                emit(ts: ts, type: "AppHang", message: String(format: "Main thread unresponsive for %.0f ms", ms),
                     stack: String(decoding: hang.callStackTree.jsonRepresentation(), as: UTF8.self),
                     source: "anr", isCrash: false, extra: ["hang.duration_ms": String(format: "%.0f", ms)])
            }
        }
    }

    private func emit(ts: Int64, type: String, message: String, stack: String, source: String, isCrash: Bool, extra: [String: String]) {
        var attrs = extra
        attrs["metrickit"] = "true"
        attrs["stack.format"] = "metrickit-json"
        let attributes = attrs
        core.submit(.error, ts: ts) { e in
            e.error = RumErrorPayload(type: type, message: message, stack: stack, source: source,
                                      isCrash: isCrash, thread: nil, fingerprint: nil)
            e.attributes = attributes
        }
    }
}
#endif
