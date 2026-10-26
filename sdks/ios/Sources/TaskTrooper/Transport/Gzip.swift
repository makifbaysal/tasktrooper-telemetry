import Foundation
#if canImport(TaskTrooperCrashC)
import TaskTrooperCrashC
#endif

enum Gzip {
    static func compress(_ data: Data) -> Data? {
        data.withUnsafeBytes { (raw: UnsafeRawBufferPointer) -> Data? in
            var out: UnsafeMutablePointer<UInt8>?
            var outLen = 0
            let base = raw.bindMemory(to: UInt8.self).baseAddress
            guard tt_gzip(base, raw.count, &out, &outLen) == 0, let out else { return nil }
            defer { free(out) }
            return Data(bytes: out, count: outLen)
        }
    }

    static func decompress(_ data: Data) -> Data? {
        data.withUnsafeBytes { (raw: UnsafeRawBufferPointer) -> Data? in
            var out: UnsafeMutablePointer<UInt8>?
            var outLen = 0
            let base = raw.bindMemory(to: UInt8.self).baseAddress
            guard tt_gunzip(base, raw.count, &out, &outLen) == 0, let out else { return nil }
            defer { free(out) }
            return Data(bytes: out, count: outLen)
        }
    }
}
