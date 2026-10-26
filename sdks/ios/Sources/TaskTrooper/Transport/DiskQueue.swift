import Foundation

/// Offline queue: one gzip'd batch per file, oldest first, total capped (FIFO drop) — contracts.md §9.
/// Confined to the SDK queue.
final class DiskQueue {
    let directory: URL
    let maxBytes: Int
    private let fm = FileManager.default
    private var seq = 0

    init?(directory: URL, maxBytes: Int) {
        self.directory = directory
        self.maxBytes = maxBytes
        do {
            try fm.createDirectory(at: directory, withIntermediateDirectories: true)
            var dir = directory
            var values = URLResourceValues()
            values.isExcludedFromBackup = true
            try? dir.setResourceValues(values)
        } catch {
            return nil
        }
    }

    static func defaultDirectory() -> URL? {
        guard let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first else { return nil }
        return base.appendingPathComponent("TaskTrooper", isDirectory: true)
    }

    func enqueue(_ data: Data, nowMs: Int64) {
        seq = (seq + 1) % 1_000_000
        let name = String(format: "%013lld-%06d.rum.gz", nowMs, seq)
        let url = directory.appendingPathComponent(name)
        do {
            try data.write(to: url, options: .atomic)
        } catch {
            TrooperLog.debug("queue write failed: \(error)")
        }
        enforceCap()
    }

    /// Files sorted oldest first, with their sizes.
    func files() -> [(url: URL, size: Int)] {
        let urls = (try? fm.contentsOfDirectory(at: directory, includingPropertiesForKeys: [.fileSizeKey])) ?? []
        return urls
            .filter { $0.pathExtension == "gz" }
            .sorted { $0.lastPathComponent < $1.lastPathComponent }
            .map { ($0, (try? $0.resourceValues(forKeys: [.fileSizeKey]).fileSize) ?? 0) }
    }

    func oldest() -> URL? { files().first?.url }

    var totalBytes: Int { files().reduce(0) { $0 + $1.size } }

    func remove(_ url: URL) { try? fm.removeItem(at: url) }

    private func enforceCap() {
        var list = files()
        var total = list.reduce(0) { $0 + $1.size }
        while total > maxBytes, !list.isEmpty {
            let f = list.removeFirst()
            remove(f.url)
            total -= f.size
            TrooperLog.debug("queue full, dropped \(f.url.lastPathComponent)")
        }
    }
}
