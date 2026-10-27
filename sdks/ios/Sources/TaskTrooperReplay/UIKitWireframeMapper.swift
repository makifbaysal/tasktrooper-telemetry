#if canImport(UIKit) && os(iOS)
import UIKit
import ObjectiveC

/// Turns window hierarchies into a flat, back-to-front wireframe in screen points (contracts.md §23).
/// Main thread only. No pixels are read: text is masked (length kept), text fields / secure entry always,
/// images are boxes. Text inputs and web views are never descended into (their internals hold the text).
///
/// SwiftUI (best effort): hosting views contribute their CALayer fills/borders and accessibility elements
/// (text, buttons, images) when SwiftUI provides them; `.trooperUnmask()` markers unmask by area.
/// Bounded by `maxNodes` and a time budget (`truncated`). Rotation/scale transforms use bounding boxes.
@MainActor
final class UIKitWireframeMapper {
    struct Result { var nodes: [WireNode]; var truncated: Bool; var cost: TimeInterval }

    private let maskAllText: Bool
    private let maxNodes: Int
    private let budget: TimeInterval
    private var nodes: [WireNode] = []
    private var started: TimeInterval = 0
    private var checks = 0
    private var truncated = false
    private var unmaskRects: [(CGRect, Bool)] = []

    init(maskAllText: Bool, maxNodes: Int = 1500, budget: TimeInterval = 0.012) {
        self.maskAllText = maskAllText
        self.maxNodes = maxNodes
        self.budget = budget
    }

    /// `windows` back to front.
    func map(windows: [UIWindow]) -> Result {
        nodes = []
        nodes.reserveCapacity(256)
        started = CACurrentMediaTime(); checks = 0; truncated = false
        for window in windows {
            guard hasBudget else { break }
            let origin = window.frame.origin
            let frame = CGRect(origin: origin, size: window.bounds.size)
            visit(window, frame: frame, clip: frame, alpha: 1, unmask: false, depth: 0)
        }
        return Result(nodes: nodes, truncated: truncated, cost: CACurrentMediaTime() - started)
    }

    private var hasBudget: Bool {
        if truncated { return false }
        if nodes.count >= maxNodes { truncated = true; return false }
        checks += 1
        if checks & 31 == 0, CACurrentMediaTime() - started > budget { truncated = true; return false }
        return true
    }

    // MARK: - Views

    /// `frame`: the view's frame in screen points.
    private func visit(_ v: UIView, frame: CGRect, clip: CGRect, alpha parentAlpha: CGFloat, unmask parentUnmask: Bool, depth: Int) {
        guard depth <= 96, hasBudget, !v.isHidden, !v.layer.isHidden else { return }
        if v is TrooperUnmaskMarkerView { return }
        let alpha = parentAlpha * v.alpha
        guard alpha > 0.01, frame.width > 0, frame.height > 0 else { return }
        let visible = frame.intersection(clip)
        guard !visible.isNull, visible.width > 0, visible.height > 0 else { return }
        let unmask = v.trooperUnmaskState ?? parentUnmask

        let descend = emit(v, frame: frame, visible: visible, alpha: alpha, unmask: unmask)
        if Self.isHostingView(v) {
            collectMarkers(v, frame: frame, depth: 0)
            mapLayers(of: v, clip: visible, alpha: alpha)
            mapAccessibility(of: v, clip: visible, alpha: alpha, unmask: unmask)
            unmaskRects.removeAll()
        }
        guard descend else { return }
        let childClip = v.clipsToBounds ? visible : clip
        let offset = CGPoint(x: frame.minX - v.bounds.minX, y: frame.minY - v.bounds.minY)
        for sub in v.subviews {
            let f = sub.frame.offsetBy(dx: offset.x, dy: offset.y)
            visit(sub, frame: f, clip: childClip, alpha: alpha, unmask: unmask, depth: depth + 1)
        }
    }

    /// Emits the node for `v` (if it draws anything); returns false when its subviews must not be visited.
    private func emit(_ v: UIView, frame: CGRect, visible: CGRect, alpha: CGFloat, unmask: Bool) -> Bool {
        let traits = v.traitCollection
        var bg = Self.rgba(v.backgroundColor, traits)
        let border = v.layer.borderWidth > 0 ? v.layer.borderColor.flatMap { Self.rgba(UIColor(cgColor: $0), traits) } : nil
        if bg == nil, let c = v.layer.backgroundColor { bg = Self.rgba(UIColor(cgColor: c), traits) }
        let radius = Double(v.layer.cornerRadius)
        var kind: String
        var text: String?
        var input = false, secure = false
        var fg: UInt32?
        var descend = true

        switch v {
        case let tf as UITextField:
            kind = "input"; descend = false
            fg = Self.rgba(tf.textColor, traits)
            secure = tf.isSecureTextEntry
            if let t = tf.text, !t.isEmpty { text = t; input = true } else { text = tf.placeholder ?? tf.attributedPlaceholder?.string }
        case let tv as UITextView:
            kind = tv.isEditable ? "input" : "text"; descend = false
            fg = Self.rgba(tv.textColor, traits)
            text = tv.text; input = tv.isEditable; secure = tv.isSecureTextEntry
        case let l as UILabel:
            kind = "text"
            fg = Self.rgba(l.textColor, traits)
            text = l.attributedText?.string ?? l.text
            if (text ?? "").isEmpty, bg == nil { return true }
        case let s as UISwitch:
            kind = "switch"; descend = false
            if s.isOn { bg = Self.rgba(s.onTintColor ?? .systemGreen, traits) }
        case is UISegmentedControl, is UIButton:
            kind = "button"
        case is UIImageView:
            kind = frame.width <= 48 && frame.height <= 48 ? "icon" : "image"
        case is UIScrollView:
            kind = "list"
        default:
            if Self.isWebView(v) { kind = "webview"; descend = false }
            else if bg == nil, border == nil { return true } // plain container: nothing to draw
            else { kind = "view" }
        }
        let m = Masking.apply(text, input: input, secure: secure, maskAll: maskAllText, unmask: unmask)
        append(id: Self.id(v), frame: frame, visible: visible, kind: kind, bg: bg, fg: fg, radius: radius, border: border,
               text: m.text, masked: m.masked, alpha: alpha)
        return descend
    }

    private func append(id: Int, frame: CGRect, visible: CGRect, kind: String, bg: UInt32?, fg: UInt32?, radius: Double,
                        border: UInt32?, text: String?, masked: Bool, alpha: CGFloat) {
        let clipped = visible.minX > frame.minX + 0.5 || visible.minY > frame.minY + 0.5
            || visible.maxX < frame.maxX - 0.5 || visible.maxY < frame.maxY - 0.5
        nodes.append(WireNode(
            id: id, x: Double(frame.minX), y: Double(frame.minY), w: Double(frame.width), h: Double(frame.height), kind: kind,
            bg: bg.flatMap { WireColor.isTransparent($0) ? nil : $0 }, fg: fg, radius: radius, border: border,
            text: text, masked: masked, alpha: Double(alpha),
            clip: clipped ? [Double(visible.minX), Double(visible.minY), Double(visible.width), Double(visible.height)] : nil))
    }

    // MARK: - SwiftUI (best effort)

    private func collectMarkers(_ v: UIView, frame: CGRect, depth: Int) {
        guard depth < 64 else { return }
        let offset = CGPoint(x: frame.minX - v.bounds.minX, y: frame.minY - v.bounds.minY)
        for sub in v.subviews where !sub.isHidden {
            let f = sub.frame.offsetBy(dx: offset.x, dy: offset.y)
            if let marker = sub as? TrooperUnmaskMarkerView { unmaskRects.append((f, marker.unmask)) }
            else if !Self.isHostingView(sub) { collectMarkers(sub, frame: f, depth: depth + 1) }
        }
    }

    /// Smallest marker containing `rect` decides; nil = no marker.
    private func markerUnmask(for rect: CGRect) -> Bool? {
        var best: (area: CGFloat, unmask: Bool)?
        for (r, u) in unmaskRects where r.insetBy(dx: -1, dy: -1).contains(rect) {
            let area = r.width * r.height
            if best == nil || area < best!.area { best = (area, u) }
        }
        return best?.unmask
    }

    /// Fills and borders of the hosting view's own (non-view) sublayers.
    private func mapLayers(of host: UIView, clip: CGRect, alpha: CGFloat) {
        guard let window = host.window else { return }
        var count = 0
        func walk(_ layer: CALayer, depth: Int) {
            guard depth < 24, count < 400, hasBudget else { return }
            for sub in layer.sublayers ?? [] {
                if sub.delegate is UIView || sub.isHidden || sub.opacity < 0.01 { continue }
                count += 1
                let bg = sub.backgroundColor.flatMap { Self.rgba(UIColor(cgColor: $0), host.traitCollection) }
                let border = sub.borderWidth > 0 ? sub.borderColor.flatMap { Self.rgba(UIColor(cgColor: $0), host.traitCollection) } : nil
                if bg.map({ !WireColor.isTransparent($0) }) == true || border != nil {
                    let r = sub.convert(sub.bounds, to: window.layer).offsetBy(dx: window.frame.minX, dy: window.frame.minY)
                    let visible = r.intersection(clip)
                    if !visible.isNull, visible.width > 0, visible.height > 0 {
                        append(id: Self.id(sub), frame: r, visible: visible, kind: "view", bg: bg, fg: nil,
                               radius: Double(sub.cornerRadius), border: border, text: nil, masked: false,
                               alpha: alpha * CGFloat(sub.opacity))
                    }
                }
                walk(sub, depth: depth + 1)
            }
        }
        walk(host.layer, depth: 0)
    }

    /// Text / buttons / images from the accessibility elements SwiftUI exposes (frames are in screen points).
    private func mapAccessibility(of host: UIView, clip: CGRect, alpha: CGFloat, unmask: Bool) {
        var count = 0
        func children(_ o: NSObject) -> [NSObject] {
            if let els = o.accessibilityElements { return els.compactMap { $0 as? NSObject } }
            let n = o.accessibilityElementCount()
            guard n != NSNotFound, n > 0 else { return [] }
            return (0..<min(n, 200)).compactMap { o.accessibilityElement(at: $0) as? NSObject }
        }
        func walk(_ o: NSObject, depth: Int) {
            guard depth < 16, count < 300, hasBudget else { return }
            for el in children(o) {
                if el is UIView { continue } // real views are mapped by the view traversal
                count += 1
                let kids = children(el)
                if kids.isEmpty {
                    emitElement(el, clip: clip, alpha: alpha, unmask: unmask)
                } else {
                    walk(el, depth: depth + 1)
                }
            }
        }
        walk(host, depth: 0)
    }

    private func emitElement(_ el: NSObject, clip: CGRect, alpha: CGFloat, unmask: Bool) {
        let frame = el.accessibilityFrame
        guard frame.width > 0, frame.height > 0 else { return }
        let visible = frame.intersection(clip)
        guard !visible.isNull, visible.width > 0, visible.height > 0 else { return }
        let t = el.accessibilityTraits
        let label = el.accessibilityLabel
        let kind: String
        var input = false
        if t.contains(.searchField) || t.contains(.keyboardKey) { kind = "input"; input = true }
        else if t.contains(.button) || t.contains(.link) || t.contains(.tabBar) { kind = "button" }
        else if t.contains(.adjustable) { kind = "switch" }
        else if t.contains(.image) { kind = frame.width <= 48 && frame.height <= 48 ? "icon" : "image" }
        else if let label, !label.isEmpty { kind = "text" }
        else { return }
        let um = markerUnmask(for: frame) ?? unmask
        let text = kind == "image" || kind == "icon" ? nil : label
        let m = Masking.apply(text, input: input, secure: false, maskAll: maskAllText, unmask: um)
        append(id: Self.id(el), frame: frame, visible: visible, kind: kind, bg: nil, fg: nil, radius: 0, border: nil,
               text: m.text, masked: m.masked, alpha: alpha)
    }

    // MARK: - Helpers

    static func id(_ o: AnyObject) -> Int { Int(truncatingIfNeeded: UInt(bitPattern: ObjectIdentifier(o).hashValue) >> 4) & 0x7fff_ffff }

    static func rgba(_ color: UIColor?, _ traits: UITraitCollection) -> UInt32? {
        guard let color else { return nil }
        let c = color.resolvedColor(with: traits)
        var r: CGFloat = 0, g: CGFloat = 0, b: CGFloat = 0, a: CGFloat = 0
        if c.getRed(&r, green: &g, blue: &b, alpha: &a) { return WireColor.rgba(Double(r), Double(g), Double(b), Double(a)) }
        var w: CGFloat = 0
        if c.getWhite(&w, alpha: &a) { return WireColor.rgba(Double(w), Double(w), Double(w), Double(a)) }
        return nil // pattern / unsupported color space
    }

    private static var hostingCache: [ObjectIdentifier: Bool] = [:]
    private static let webViewClass: AnyClass? = NSClassFromString("WKWebView")

    static func isHostingView(_ v: UIView) -> Bool {
        let id = ObjectIdentifier(type(of: v))
        if let cached = hostingCache[id] { return cached }
        let r = NSStringFromClass(type(of: v)).contains("HostingView")
        hostingCache[id] = r
        return r
    }

    static func isWebView(_ v: UIView) -> Bool {
        guard let cls = webViewClass else { return false }
        return v.isKind(of: cls)
    }
}

// MARK: - Unmask

@MainActor
enum ReplayKeys {
    static var unmask: UInt8 = 0
}

public extension UIView {
    /// Session replay: record the text of this view and its subtree unmasked (`true`), or force masking inside
    /// an unmasked parent (`false`). Text fields and secure entry are always masked.
    var trooperUnmask: Bool {
        get { trooperUnmaskState ?? false }
        set { objc_setAssociatedObject(self, &ReplayKeys.unmask, NSNumber(value: newValue), .OBJC_ASSOCIATION_RETAIN_NONATOMIC) }
    }

    internal var trooperUnmaskState: Bool? {
        (objc_getAssociatedObject(self, &ReplayKeys.unmask) as? NSNumber)?.boolValue
    }
}

/// Invisible view placed by SwiftUI `.trooperUnmask()`; its area is unmasked in the wireframe.
final class TrooperUnmaskMarkerView: UIView {
    var unmask = true

    override init(frame: CGRect) {
        super.init(frame: frame)
        isUserInteractionEnabled = false
        isAccessibilityElement = false
        backgroundColor = .clear
    }

    required init?(coder: NSCoder) { nil }
}
#endif
