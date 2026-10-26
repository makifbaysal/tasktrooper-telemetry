import Foundation
#if canImport(TaskTrooper)
import TaskTrooper
#endif

/// Hybrid bridge logic (contracts.md §9) behind the `TrooperRum` module. Forwards to the TaskTrooper iOS SDK when it is
/// compiled in (`canImport(TaskTrooper)`); otherwise context calls are no-ops and the process start comes from sysctl.
@objc(TrooperRumBridge)
public final class TrooperRumBridge: NSObject {
    /// JS → native SDK. Only non-empty string values are forwarded.
    @objc public static func setContext(_ ctx: NSDictionary?) {
        #if canImport(TaskTrooper)
        var d: [String: Any] = [:]
        for (k, v) in ctx ?? [:] {
            if let k = k as? String, let v = v as? String, !v.isEmpty { d[k] = v }
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
