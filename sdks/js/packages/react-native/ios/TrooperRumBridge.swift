import Foundation
#if canImport(TaskTrooper)
import TaskTrooper
#endif
// Session replay: a separate module with SwiftPM; with CocoaPods the `TaskTrooper/Replay` subspec compiles into the
// `TaskTrooper` module, and the podspec defines TROOPER_RN_REPLAY when it pulls that subspec in.
#if canImport(TaskTrooperReplay)
import TaskTrooperReplay
#endif
#if canImport(TaskTrooperReplay) || (TROOPER_RN_REPLAY && canImport(TaskTrooper))
private let replayAvailable = true
#else
private let replayAvailable = false
#endif

/// Hybrid bridge logic (contracts.md §9) behind the `TrooperRum` module. Forwards to the TaskTrooper iOS SDK when it is
/// compiled in (`canImport(TaskTrooper)`); otherwise context calls are no-ops and the process start comes from sysctl.
@objc(TrooperRumBridge)
public final class TrooperRumBridge: NSObject {
    /// JS → native SDK. Non-empty string values and the boolean `sampled` (JS RUM sampling decision, contracts.md §9)
    /// are forwarded; `TrooperHybridContext(dictionary:)` reads `sampled` and native uses it for the adopted session.
    @objc public static func setContext(_ ctx: NSDictionary?) {
        #if canImport(TaskTrooper)
        var d: [String: Any] = [:]
        for (k, v) in ctx ?? [:] {
            guard let k = k as? String else { continue }
            if let v = v as? String, !v.isEmpty { d[k] = v }
            else if k == "sampled", let v = v as? NSNumber { d[k] = v.boolValue }
        }
        guard let context = TrooperHybridContext(dictionary: d) else { return }
        TaskTrooper.setHybridContext(context)
        #endif
    }

    /// Native SDK context, or nil without the SDK / before `TaskTrooper.start`.
    @objc public static func getContext() -> NSDictionary? {
        #if canImport(TaskTrooper)
        if let ctx = TaskTrooper.hybridContext() { return ctx.dictionary as NSDictionary }
        #endif
        return nil
    }

    // MARK: - Session replay (contracts.md §23 "Hibrit uygulamalar")

    /// Installs the native wireframe replay with the JS options (missing keys = native defaults). The RUM session is the
    /// JS one (`setContext` → `TaskTrooper.setHybridContext`). False when the replay module is not compiled in.
    @objc public static func startReplay(_ options: NSDictionary?) -> Bool {
        guard replayAvailable else { return false }
        #if canImport(TaskTrooperReplay) || (TROOPER_RN_REPLAY && canImport(TaskTrooper))
        var o = TrooperReplayOptions()
        if let v = options?["sampleRate"] as? NSNumber, (0...1).contains(v.doubleValue) { o.sampleRate = v.doubleValue }
        if let v = options?["onErrorSampleRate"] as? NSNumber, (0...1).contains(v.doubleValue) { o.onErrorSampleRate = v.doubleValue }
        if let v = options?["maskAllText"] as? NSNumber { o.maskAllText = v.boolValue }
        TrooperReplay.install(o)
        #endif
        return true
    }

    /// Stops native replay (seals and sends the open segment); `startReplay` works again afterwards.
    @objc public static func stopReplay() -> Bool {
        #if canImport(TaskTrooperReplay) || (TROOPER_RN_REPLAY && canImport(TaskTrooper))
        TrooperReplay.stop()
        return true
        #else
        return false
        #endif
    }

    @objc public static func isReplayRecording() -> Bool {
        #if canImport(TaskTrooperReplay) || (TROOPER_RN_REPLAY && canImport(TaskTrooper))
        return TrooperReplay.isRecording
        #else
        return false
        #endif
    }

    /// A JS ERROR: the native error observer only sees native errors, so flush the on-error buffer explicitly.
    @objc public static func notifyReplayError() {
        #if canImport(TaskTrooperReplay) || (TROOPER_RN_REPLAY && canImport(TaskTrooper))
        TrooperReplay.notifyError()
        #endif
    }

    /// Unix epoch ms of process start (native SDK value if > 0, else sysctl), or nil if unknown.
    @objc public static func processStartTime() -> NSNumber? {
        #if canImport(TaskTrooper)
        let v = TaskTrooper.processStartTimeMs()
        if v > 0 { return NSNumber(value: Double(v)) }
        #endif
        return sysctlProcessStartMs().map { NSNumber(value: $0) }
    }

    private static func sysctlProcessStartMs() -> Double? {
        var mib: [Int32] = [CTL_KERN, KERN_PROC, KERN_PROC_PID, getpid()]
        var info = kinfo_proc()
        var size = MemoryLayout<kinfo_proc>.stride
        guard sysctl(&mib, u_int(mib.count), &info, &size, nil, 0) == 0, size > 0 else { return nil }
        let t = info.kp_proc.p_un.__p_starttime
        let ms = Double(t.tv_sec) * 1000 + Double(t.tv_usec) / 1000
        return ms > 0 ? ms : nil
    }
}
