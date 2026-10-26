import Flutter
import Foundation
#if canImport(TaskTrooper)
import TaskTrooper
#endif

/// Adapter to a native TaskTrooper SDK the plugin cannot import itself (e.g. TaskTrooper added to the
/// app target via SPM). Register it with `TrooperFlutterPlugin.hybridBridge` before `runApp`.
public protocol TrooperFlutterHybridBridge: AnyObject {
    /// Dart → native: `{session_id, anonymous_id, view_id, view_name, user_id}`.
    func setContext(_ context: [String: Any])
    /// Native session context, nil before the native SDK started.
    func getContext() -> [String: String]?
    /// Unix epoch ms of process start, or 0 if unknown.
    func processStartTimeMs() -> Int64
}

/// Flutter side of the hybrid bridge (contracts.md §9), channel `dev.tasktrooper.telemetry/rum`.
/// Forwards to the native TaskTrooper SDK when present; `getProcessStartTime` always works (sysctl).
public class TrooperFlutterPlugin: NSObject, FlutterPlugin {
    public static let channelName = "dev.tasktrooper.telemetry/rum"

    /// Optional explicit bridge; takes precedence over the compiled-in TaskTrooper integration.
    public static var hybridBridge: TrooperFlutterHybridBridge?

    public static func register(with registrar: FlutterPluginRegistrar) {
        let channel = FlutterMethodChannel(name: channelName, binaryMessenger: registrar.messenger())
        registrar.addMethodCallDelegate(TrooperFlutterPlugin(), channel: channel)
    }

    public func handle(_ call: FlutterMethodCall, result: @escaping FlutterResult) {
        switch call.method {
        case "setContext":
            if let args = call.arguments as? [String: Any] { Self.setContext(args) }
            result(nil)
        case "getContext":
            result(Self.getContext())
        case "getProcessStartTime":
            let ms = Self.processStartTimeMs()
            result(ms > 0 ? NSNumber(value: ms) : nil)
        default:
            result(FlutterMethodNotImplemented)
        }
    }

    static func setContext(_ args: [String: Any]) {
        if let bridge = hybridBridge {
            bridge.setContext(args)
            return
        }
        #if canImport(TaskTrooper)
        if let ctx = TrooperHybridContext(dictionary: args) {
            TaskTrooper.setHybridContext(ctx)
        }
        #endif
    }

    static func getContext() -> [String: String]? {
        if let bridge = hybridBridge {
            return bridge.getContext()
        }
        #if canImport(TaskTrooper)
        return TaskTrooper.hybridContext()?.dictionary
        #else
        return nil
        #endif
    }

    /// Native SDK value if > 0, else the kernel's process start time.
    static func processStartTimeMs() -> Int64 {
        if let ms = hybridBridge?.processStartTimeMs(), ms > 0 { return ms }
        #if canImport(TaskTrooper)
        let fromSdk = TaskTrooper.processStartTimeMs()
        if fromSdk > 0 { return fromSdk }
        #endif
        return sysctlProcessStartTimeMs()
    }

    /// `sysctl(KERN_PROC_PID)` → `kp_proc.p_starttime` (timeval) as unix epoch ms; 0 on failure.
    static func sysctlProcessStartTimeMs() -> Int64 {
        var info = kinfo_proc()
        var size = MemoryLayout<kinfo_proc>.stride
        var mib: [Int32] = [CTL_KERN, KERN_PROC, KERN_PROC_PID, getpid()]
        let rc = mib.withUnsafeMutableBufferPointer { sysctl($0.baseAddress, 4, &info, &size, nil, 0) }
        guard rc == 0, size > 0 else { return 0 }
        let tv = info.kp_proc.p_un.__p_starttime
        return Int64(tv.tv_sec) * 1000 + Int64(tv.tv_usec) / 1000
    }
}
