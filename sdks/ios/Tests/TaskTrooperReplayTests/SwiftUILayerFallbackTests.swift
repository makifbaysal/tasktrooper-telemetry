#if canImport(UIKit) && os(iOS)
import XCTest
import UIKit
@testable import TaskTrooperReplay

/// Stand-ins for SwiftUI's private classes (matched by name, like the mapper does).
private final class FakeHostingView: UIView {}
private final class FakeCGDrawingLayer: CALayer {}

@MainActor
final class SwiftUILayerFallbackTests: XCTestCase {
    private func image() -> CGImage {
        UIGraphicsImageRenderer(size: CGSize(width: 2, height: 2)).image { _ in }.cgImage!
    }

    private func host(in w: UIWindow) -> (UIView, CALayer, CALayer, CALayer) {
        let host = FakeHostingView(frame: CGRect(x: 0, y: 100, width: 390, height: 600))
        let text = FakeCGDrawingLayer()
        text.frame = CGRect(x: 20, y: 10, width: 160, height: 20)
        text.contents = image()
        let icon = CALayer()
        icon.frame = CGRect(x: 200, y: 10, width: 18, height: 18)
        icon.contents = image()
        let fill = CALayer()
        fill.frame = CGRect(x: 20, y: 50, width: 60, height: 40)
        fill.backgroundColor = UIColor.blue.cgColor
        fill.cornerRadius = 10
        [text, icon, fill].forEach(host.layer.addSublayer)
        w.addSubview(host)
        return (host, text, icon, fill)
    }

    private func window() -> UIWindow {
        let w = UIWindow(frame: CGRect(x: 0, y: 0, width: 390, height: 844))
        w.isHidden = false
        return w
    }

    /// Regression: without an assistive technology running SwiftUI exposes no accessibility elements, and the
    /// SwiftUI home screen of the sample recorded only the window and the SecureField (texts and buttons missing).
    func testContentLayersAreMappedWithoutAccessibility() throws {
        let w = window()
        let (_, text, icon, fill) = host(in: w)
        XCTAssertTrue(UIKitWireframeMapper.isHostingView(w.subviews[0]))
        let r = UIKitWireframeMapper(maskAllText: false).map(windows: [w])

        let t = try XCTUnwrap(r.nodes.first { $0.id == UIKitWireframeMapper.id(text) ^ 1 })
        XCTAssertEqual(t.kind, "text")
        XCTAssertEqual([t.x, t.y, t.w, t.h], [20, 110, 160, 20])
        XCTAssertTrue(t.masked, "the string is unknown: always a masked placeholder, even with maskAllText off")
        XCTAssertEqual(t.text, String(repeating: "•", count: 16))

        let i = try XCTUnwrap(r.nodes.first { $0.id == UIKitWireframeMapper.id(icon) ^ 1 })
        XCTAssertEqual(i.kind, "icon")

        let f = try XCTUnwrap(r.nodes.first { $0.id == UIKitWireframeMapper.id(fill) })
        XCTAssertEqual(f.kind, "view")
        XCTAssertEqual(f.bg, 0x0000ffff)
        XCTAssertEqual(f.radius, 10)
    }

    func testAccessibilityElementsWinOverContentLayers() throws {
        let w = window()
        let (host, text, _, fill) = host(in: w)
        let el = UIAccessibilityElement(accessibilityContainer: host)
        el.accessibilityLabel = "Order #2001"
        el.accessibilityTraits = .staticText
        el.accessibilityFrame = CGRect(x: 20, y: 110, width: 160, height: 20)
        host.accessibilityElements = [el]
        let r = UIKitWireframeMapper(maskAllText: false).map(windows: [w])

        XCTAssertNil(r.nodes.first { $0.id == UIKitWireframeMapper.id(text) ^ 1 })
        let t = try XCTUnwrap(r.nodes.first { $0.kind == "text" })
        XCTAssertEqual(t.text, "Order #2001")
        XCTAssertFalse(t.masked)
        // Fills still come first (back to front).
        let fi = try XCTUnwrap(r.nodes.firstIndex { $0.id == UIKitWireframeMapper.id(fill) })
        XCTAssertLessThan(fi, try XCTUnwrap(r.nodes.firstIndex { $0.kind == "text" }))
    }

    /// Regression: with accessibility on (XCUITest, VoiceOver) walking SwiftUI accessibility elements took > 12 ms per
    /// capture on a List and truncated every frame. With everything masked the strings are not needed: layers only.
    func testAccessibilityIsNotWalkedWhenAllTextIsMasked() throws {
        let w = window()
        let (host, text, _, _) = host(in: w)
        let el = UIAccessibilityElement(accessibilityContainer: host)
        el.accessibilityLabel = "Order #2001"
        el.accessibilityFrame = CGRect(x: 20, y: 110, width: 160, height: 20)
        host.accessibilityElements = [el]
        let r = UIKitWireframeMapper(maskAllText: true).map(windows: [w])
        XCTAssertNotNil(r.nodes.first { $0.id == UIKitWireframeMapper.id(text) ^ 1 })
        XCTAssertNil(r.nodes.first { $0.id == UIKitWireframeMapper.id(el) })

        host.trooperUnmask = true // an unmasked subtree needs the strings again
        let u = UIKitWireframeMapper(maskAllText: true).map(windows: [w])
        XCTAssertEqual(u.nodes.first { $0.id == UIKitWireframeMapper.id(el) }?.text, "Order #2001")
    }

    func testPlaceholderText() {
        XCTAssertEqual(UIKitWireframeMapper.placeholderText(width: 160, height: 20), String(repeating: "•", count: 16))
        let two = UIKitWireframeMapper.placeholderText(width: 110, height: 44).split(separator: " ")
        XCTAssertEqual(two.count, 2)
        XCTAssertEqual(UIKitWireframeMapper.placeholderText(width: 0.5, height: 3), "•")
        XCTAssertLessThanOrEqual(UIKitWireframeMapper.placeholderText(width: 10_000, height: 2_000).count, Masking.maxText + 12)
        XCTAssertEqual(UIKitWireframeMapper.contentKind(layerClass: "CGDrawingLayer"), .text)
        XCTAssertEqual(UIKitWireframeMapper.contentKind(layerClass: "SwiftUI.ImageLayer"), .image)
    }
}
#endif
