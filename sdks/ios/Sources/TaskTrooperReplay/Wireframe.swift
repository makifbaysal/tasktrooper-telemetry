import Foundation

/// One wireframe node (contracts.md §23): screen coordinates in points, back-to-front order.
/// Colors are `0xRRGGBBAA`; serialized as `#rrggbb` (opaque) or `#rrggbbaa`.
struct WireNode: Equatable, Sendable {
    var id: Int
    var x: Double
    var y: Double
    var w: Double
    var h: Double
    var kind: String
    var bg: UInt32? = nil
    var fg: UInt32? = nil
    var radius: Double = 0
    var border: UInt32? = nil
    var text: String? = nil
    var masked = false
    var alpha: Double = 1
    /// Visible part `[x, y, w, h]` when clipped by an ancestor; nil when fully visible.
    var clip: [Double]? = nil

    func write(_ j: inout JSONText) {
        j.beginObject()
        j.key("id"); j.int(id)
        j.key("x"); j.number(Self.round1(x))
        j.key("y"); j.number(Self.round1(y))
        j.key("w"); j.number(Self.round1(w))
        j.key("h"); j.number(Self.round1(h))
        j.key("kind"); j.string(kind)
        if let bg { j.key("bg"); j.string(WireColor.hex(bg)) }
        if let fg { j.key("fg"); j.string(WireColor.hex(fg | 0xff)) }          // contract: fg is #rrggbb
        if radius > 0 { j.key("radius"); j.number(Self.round1(radius)) }
        if let border { j.key("border"); j.string(WireColor.hex(border | 0xff)) }
        if let text { j.key("text"); j.string(text) }
        if masked { j.key("masked"); j.bool(true) }
        if alpha < 0.999 { j.key("alpha"); j.number((alpha * 100).rounded() / 100) }
        if let clip {
            j.key("clip"); j.beginArray()
            for v in clip { j.number(Self.round1(v)) }
            j.endArray()
        }
        j.endObject()
    }

    static func round1(_ v: Double) -> Double { (v * 10).rounded() / 10 }
}

enum WireColor {
    /// `0xRRGGBBAA` from components in 0...1.
    static func rgba(_ r: Double, _ g: Double, _ b: Double, _ a: Double) -> UInt32 {
        func c(_ v: Double) -> UInt32 { UInt32((min(max(v, 0), 1) * 255).rounded()) }
        return c(r) << 24 | c(g) << 16 | c(b) << 8 | c(a)
    }

    static func hex(_ rgba: UInt32) -> String {
        let a = rgba & 0xff
        return a == 0xff ? String(format: "#%06x", rgba >> 8) : String(format: "#%08x", rgba)
    }

    static func isTransparent(_ rgba: UInt32) -> Bool { rgba & 0xff == 0 }
}

/// Text masking: every non-whitespace character becomes `•`, preserving length and word shape.
enum Masking {
    static let maxText = 300

    static func mask(_ s: String) -> String {
        var out = ""
        out.reserveCapacity(min(s.count, maxText))
        for ch in s.prefix(maxText) { out.append(ch.isWhitespace ? ch : "•") }
        return out
    }

    /// Inputs and secure fields are always masked; other text when `maskAll` unless unmasked.
    static func apply(_ raw: String?, input: Bool, secure: Bool, maskAll: Bool, unmask: Bool) -> (text: String?, masked: Bool) {
        guard let raw, !raw.isEmpty else { return (nil, false) }
        if input || secure || (maskAll && !unmask) { return (mask(raw), true) }
        return (String(raw.prefix(maxText)), false)
    }
}

/// Minimal, ordered JSON writer (JSONEncoder is slower and does not keep key order).
struct JSONText {
    private(set) var text = ""
    private var needComma = false

    init(capacity: Int = 256) { text.reserveCapacity(capacity) }

    private mutating func comma() { if needComma { text.append(",") } }
    mutating func beginObject() { comma(); text.append("{"); needComma = false }
    mutating func endObject() { text.append("}"); needComma = true }
    mutating func beginArray() { comma(); text.append("["); needComma = false }
    mutating func endArray() { text.append("]"); needComma = true }
    mutating func key(_ k: String) { comma(); appendQuoted(k); text.append(":"); needComma = false }
    mutating func string(_ s: String) { comma(); appendQuoted(s); needComma = true }
    mutating func int(_ v: Int) { comma(); text.append(String(v)); needComma = true }
    mutating func int64(_ v: Int64) { comma(); text.append(String(v)); needComma = true }
    mutating func bool(_ v: Bool) { comma(); text.append(v ? "true" : "false"); needComma = true }
    mutating func number(_ v: Double) {
        comma()
        if !v.isFinite { text.append("0") }
        else if v == v.rounded(), abs(v) < 1e15 { text.append(String(Int64(v))) }
        else { text.append(String(v)) }
        needComma = true
    }
    mutating func raw(_ json: String) { comma(); text.append(json); needComma = true }

    private mutating func appendQuoted(_ s: String) {
        text.append("\"")
        for u in s.unicodeScalars {
            switch u {
            case "\"": text.append("\\\"")
            case "\\": text.append("\\\\")
            case "\n": text.append("\\n")
            case "\r": text.append("\\r")
            case "\t": text.append("\\t")
            default:
                if u.value < 0x20 || u.value == 0x2028 || u.value == 0x2029 {
                    text.append(String(format: "\\u%04x", u.value))
                } else {
                    text.unicodeScalars.append(u)
                }
            }
        }
        text.append("\"")
    }
}

/// Wireframe event JSON (contracts.md §23 `events[]`).
enum WireEvents {
    static func screen(ts: Int64, width: Int, height: Int, density: Double, name: String?) -> String {
        var j = JSONText(capacity: 96)
        j.beginObject()
        j.key("type"); j.string("screen")
        j.key("ts"); j.int64(ts)
        j.key("width"); j.int(width)
        j.key("height"); j.int(height)
        j.key("density"); j.number((density * 100).rounded() / 100)
        if let name, !name.isEmpty { j.key("name"); j.string(name) }
        j.endObject()
        return j.text
    }

    static func nodesKey(_ nodes: [WireNode]) -> String {
        var j = JSONText(capacity: nodes.count * 96 + 2)
        j.beginArray()
        for n in nodes { n.write(&j) }
        j.endArray()
        return j.text
    }

    static func frame(ts: Int64, nodesKey: String) -> String {
        "{\"type\":\"frame\",\"ts\":\(ts),\"nodes\":\(nodesKey)}"
    }

    static func touch(ts: Int64, x: Double, y: Double, phase: String) -> String {
        var j = JSONText(capacity: 80)
        j.beginObject()
        j.key("type"); j.string("touch")
        j.key("ts"); j.int64(ts)
        j.key("x"); j.number(WireNode.round1(x))
        j.key("y"); j.number(WireNode.round1(y))
        j.key("phase"); j.string(phase)
        j.endObject()
        return j.text
    }

    static func keyboard(ts: Int64, visible: Bool) -> String {
        "{\"type\":\"keyboard\",\"ts\":\(ts),\"visible\":\(visible)}"
    }
}
