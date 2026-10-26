import Foundation
#if canImport(TaskTrooperCrashC)
import TaskTrooperCrashC
#endif

struct BinaryImage: Equatable, Sendable {
    var loadAddress: UInt64
    var size: UInt64
    var uuid: String
    var path: String

    var name: String { (path as NSString).lastPathComponent }
}

/// One non-crashed thread captured by the signal handler.
struct ThreadStack: Equatable, Sendable {
    var name: String
    var frames: [UInt64]
}

/// Apple crash-report style text ("Thread N Crashed:" + frames + "Binary Images:"), the format
/// services/symbolicator symbolicates with dSYMs (UUID + load address).
enum AppleStackFormatter {
    struct Section {
        var index: Int
        var crashed: Bool
        var name: String?
        var frames: [UInt64]
    }

    static func format(_ sections: [Section], images: [BinaryImage]) -> String {
        let used = images.filter { img in sections.contains { $0.frames.contains { f in f >= img.loadAddress && f < img.loadAddress + img.size } } }
        var out = ""
        for (n, section) in sections.enumerated() {
            if n > 0 { out += "\n" }
            if let name = section.name { out += "Thread \(section.index) name:  \(name)\n" }
            out += "Thread \(section.index)\(section.crashed ? " Crashed" : ""):\n"
            for (i, f) in section.frames.enumerated() {
                let img = used.first { f >= $0.loadAddress && f < $0.loadAddress + $0.size }
                let imgName = img?.name ?? "???"
                let pad = imgName.padding(toLength: max(imgName.count, 30), withPad: " ", startingAt: 0)
                if let img {
                    out += String(format: "%-4d%@ 0x%016llx 0x%llx + %llu\n", i, pad, f, img.loadAddress, f - img.loadAddress)
                } else {
                    out += String(format: "%-4d%@ 0x%016llx\n", i, pad, f)
                }
            }
        }
        out += "\nBinary Images:\n"
        for img in used {
            out += String(format: "0x%llx - 0x%llx %@ %@ <%@> %@\n",
                          img.loadAddress, img.loadAddress + max(img.size, 1) - 1, img.name, CrashReport.arch, img.uuid, img.path)
        }
        return out
    }

    /// Binary images currently loaded in this process (tracked by the C layer).
    static func loadedImages() -> [BinaryImage] {
        tt_images_register()
        var result: [BinaryImage] = []
        let count = tt_image_count()
        result.reserveCapacity(Int(count))
        for i in 0..<count {
            var load: UInt64 = 0, size: UInt64 = 0
            var uuid = [UInt8](repeating: 0, count: 16)
            var path: UnsafePointer<CChar>?
            guard tt_image_info(i, &load, &size, &uuid, &path) == 0 else { continue }
            let hex = uuid.map { String(format: "%02x", $0) }.joined()
            result.append(BinaryImage(loadAddress: load, size: size, uuid: CrashReport.formatUUID(hex),
                                      path: path.map { String(cString: $0) } ?? "?"))
        }
        return result
    }
}

/// Crash report written by the C signal/exception handler (line format `TTCRASH 1`).
struct CrashInfo: Equatable, Sendable {
    var image: String
    var message: String
}

struct CrashReport: Equatable, Sendable {
    var tsMs: Int64 = 0
    var signal: Int32 = 0
    var signalName = ""
    var code: Int64 = 0
    var faultAddress: UInt64 = 0
    var sessionId = ""
    var viewId = ""
    var viewName = ""
    var userId = ""
    var exceptionName: String?
    var exceptionReason: String?
    var thread = ""
    var frames: [UInt64] = []
    /// Other threads at the time of the crash.
    var threads: [ThreadStack] = []
    /// `__crash_info` annotations: (image name, message), e.g. Swift fatalError text from libswiftCore.
    var crashInfo: [CrashInfo] = []
    var images: [BinaryImage] = []
    var complete = false

    static func parse(_ text: String) -> CrashReport? {
        var lines = text.split(separator: "\n", omittingEmptySubsequences: false).map(String.init)
        guard let first = lines.first, first == "TTCRASH 1" else { return nil }
        lines.removeFirst()
        var r = CrashReport()
        for line in lines {
            let (key, value) = split(line)
            switch key {
            case "ts": r.tsMs = Int64(value) ?? 0
            case "signal":
                let (num, name) = split(value)
                r.signal = Int32(num) ?? 0
                r.signalName = name
            case "code": r.code = Int64(value) ?? 0
            case "addr": r.faultAddress = hex(value)
            case "session": r.sessionId = value
            case "view_id": r.viewId = value
            case "view_name": r.viewName = value
            case "user": r.userId = value
            case "exception_name": r.exceptionName = value
            case "exception_reason": r.exceptionReason = value.replacingOccurrences(of: "\\n", with: "\n")
            case "thread": r.thread = value
            case "frame": r.frames.append(hex(value))
            case "thread_begin": r.threads.append(ThreadStack(name: value, frames: []))
            case "tframe":
                if !r.threads.isEmpty { r.threads[r.threads.count - 1].frames.append(hex(value)) }
            case "crash_info":
                if let sep = value.range(of: ": ") {
                    r.crashInfo.append(CrashInfo(image: String(value[..<sep.lowerBound]),
                                                 message: String(value[sep.upperBound...]).replacingOccurrences(of: "\\n", with: "\n")))
                }
            case "image":
                let parts = value.split(separator: " ", maxSplits: 3).map(String.init)
                if parts.count == 4 {
                    r.images.append(BinaryImage(loadAddress: hex(parts[0]), size: hex(parts[1]), uuid: formatUUID(parts[2]), path: parts[3]))
                }
            case "end": r.complete = true
            default: break
            }
        }
        return r.tsMs > 0 ? r : nil
    }

    private static func split(_ s: String) -> (String, String) {
        guard let i = s.firstIndex(of: " ") else { return (s, "") }
        return (String(s[..<i]), String(s[s.index(after: i)...]))
    }

    private static func hex(_ s: String) -> UInt64 {
        UInt64(s.hasPrefix("0x") ? String(s.dropFirst(2)) : s, radix: 16) ?? 0
    }

    /// 32 hex chars → 8-4-4-4-12 uppercase (dSYM / dwarfdump format).
    static func formatUUID(_ s: String) -> String {
        guard s.count == 32 else { return s.uppercased() }
        let u = Array(s.uppercased())
        let groups = [8, 4, 4, 4, 12]
        var out: [String] = []
        var i = 0
        for g in groups {
            out.append(String(u[i..<i + g]))
            i += g
        }
        return out.joined(separator: "-")
    }

    var errorType: String { exceptionName ?? (signalName.isEmpty ? "Crash" : signalName) }

    /// The most relevant `__crash_info` message: libswiftCore (fatalError / precondition) first, then
    /// any other image (libsystem_c abort, libdispatch/libobjc client bugs). The simulator host's
    /// permanent "CoreSimulator …" annotation is ignored.
    var crashInfoMessage: String? {
        let relevant = crashInfo.filter { !$0.message.hasPrefix("CoreSimulator ") }
        let info = relevant.first { $0.image.hasPrefix("libswiftCore") } ?? relevant.first
        return info?.message.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    var message: String {
        if let reason = exceptionReason, !reason.isEmpty { return reason }
        if let info = crashInfoMessage, !info.isEmpty { return info }
        return String(format: "Signal %@ (code %lld) at 0x%016llx", signalName, code, faultAddress)
    }

    static var arch: String {
        #if arch(arm64)
        return "arm64"
        #elseif arch(x86_64)
        return "x86_64"
        #else
        return "unknown"
        #endif
    }

    /// Apple-style frames for every thread + "Binary Images" so the server can symbolicate with dSYMs.
    /// The crashed thread is Thread 0; the others follow in task order.
    func stackText() -> String {
        var sections = [AppleStackFormatter.Section(index: 0, crashed: true, name: thread == "main" ? "main" : nil, frames: frames)]
        for (i, t) in threads.enumerated() {
            sections.append(.init(index: i + 1, crashed: false, name: t.name == "main" ? "main" : nil, frames: t.frames))
        }
        return AppleStackFormatter.format(sections, images: images)
    }

    func errorPayload() -> RumErrorPayload {
        RumErrorPayload(type: errorType, message: message, stack: stackText(), source: "crash",
                        isCrash: true, thread: thread.isEmpty ? nil : thread, fingerprint: nil)
    }
}

enum CrashReporter {
    static let unsampledSession = "-"

    static func reportURL(in directory: URL) -> URL { directory.appendingPathComponent("crash.report") }

    /// Reads (and removes) a report left by a previous process. Call before `install`.
    static func loadPending(at url: URL) -> CrashReport? {
        guard let data = try? Data(contentsOf: url), !data.isEmpty else { return nil }
        try? FileManager.default.removeItem(at: url)
        return CrashReport.parse(String(decoding: data, as: UTF8.self))
    }

    @discardableResult
    static func install(at url: URL, signals: Bool, exceptions: Bool) -> Bool {
        url.withUnsafeFileSystemRepresentation { path in
            guard let path else { return false }
            return tt_crash_install(path, signals ? 1 : 0, exceptions ? 1 : 0) == 0
        }
    }

    static func setContext(sessionId: String, viewId: String?, viewName: String?, userId: String?) {
        tt_crash_set_context(sessionId, viewId ?? "", viewName ?? "", userId ?? "")
    }
}
