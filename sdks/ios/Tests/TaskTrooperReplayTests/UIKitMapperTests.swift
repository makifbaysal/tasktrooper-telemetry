#if canImport(UIKit) && os(iOS)
import XCTest
import UIKit
@testable import TaskTrooperReplay

@MainActor
final class UIKitMapperTests: XCTestCase {
    private func window() -> UIWindow {
        let w = UIWindow(frame: CGRect(x: 0, y: 0, width: 390, height: 844))
        w.isHidden = false
        w.backgroundColor = .white
        return w
    }

    private func map(_ w: UIWindow, maskAll: Bool = true, maxNodes: Int = 1500) -> UIKitWireframeMapper.Result {
        UIKitWireframeMapper(maskAllText: maskAll, maxNodes: maxNodes).map(windows: [w])
    }

    private func node(_ r: UIKitWireframeMapper.Result, _ o: AnyObject) -> WireNode? {
        r.nodes.first { $0.id == UIKitWireframeMapper.id(o) }
    }

    func testKindsBoundsColorsAndMasking() throws {
        let w = window()
        let card = UIView(frame: CGRect(x: 10, y: 100, width: 200, height: 100))
        card.backgroundColor = .blue
        card.layer.cornerRadius = 12
        card.layer.borderWidth = 1
        card.layer.borderColor = UIColor.red.cgColor
        let label = UILabel(frame: CGRect(x: 5, y: 10, width: 150, height: 20))
        label.text = "Hello World"
        label.textColor = .red
        card.addSubview(label)
        let plain = UIView(frame: CGRect(x: 0, y: 300, width: 50, height: 50)) // no background: not drawn
        let image = UIImageView(frame: CGRect(x: 0, y: 400, width: 200, height: 120))
        let icon = UIImageView(frame: CGRect(x: 300, y: 400, width: 24, height: 24))
        let toggle = UISwitch(frame: CGRect(x: 0, y: 600, width: 51, height: 31))
        toggle.isOn = true
        let button = UIButton(type: .system)
        button.frame = CGRect(x: 0, y: 700, width: 120, height: 44)
        button.setTitle("Pay now", for: .normal)
        button.layoutIfNeeded()
        [card, plain, image, icon, toggle, button].forEach(w.addSubview)

        let r = map(w)
        XCTAssertFalse(r.truncated)
        XCTAssertEqual(r.nodes.first?.kind, "view") // window background
        XCTAssertEqual(r.nodes.first?.bg, 0xffffffff)

        let c = try XCTUnwrap(node(r, card))
        XCTAssertEqual(c.kind, "view")
        XCTAssertEqual(c.bg, 0x0000ffff)
        XCTAssertEqual(c.radius, 12)
        XCTAssertEqual(c.border, 0xff0000ff)

        let l = try XCTUnwrap(node(r, label))
        XCTAssertEqual(l.kind, "text")
        XCTAssertEqual([l.x, l.y, l.w, l.h], [15, 110, 150, 20])
        XCTAssertEqual(l.text, "••••• •••••")
        XCTAssertTrue(l.masked)
        XCTAssertEqual(l.fg, 0xff0000ff)

        XCTAssertNil(node(r, plain))
        XCTAssertEqual(node(r, image)?.kind, "image")
        XCTAssertEqual(node(r, icon)?.kind, "icon")
        XCTAssertEqual(node(r, toggle)?.kind, "switch")
        XCTAssertNotNil(node(r, toggle)?.bg)
        XCTAssertEqual(node(r, toggle)?.checked, true)
        XCTAssertNil(node(r, button)?.checked)
        XCTAssertEqual(node(r, button)?.kind, "button")
        XCTAssertFalse(r.nodes.contains { $0.text == "Pay now" }, "button titles are masked by default")
        // Back to front: parent before child.
        XCTAssertLessThan(r.nodes.firstIndex(of: c)!, r.nodes.firstIndex(of: l)!)
    }

    func testInputsAreAlwaysMaskedAndNotDescended() throws {
        let w = window()
        let field = UITextField(frame: CGRect(x: 0, y: 0, width: 300, height: 44))
        field.text = "me@example.com"
        let secure = UITextField(frame: CGRect(x: 0, y: 50, width: 300, height: 44))
        secure.isSecureTextEntry = true
        secure.text = "hunter2"
        let empty = UITextField(frame: CGRect(x: 0, y: 100, width: 300, height: 44))
        empty.placeholder = "Email"
        let notes = UITextView(frame: CGRect(x: 0, y: 150, width: 300, height: 100))
        notes.text = "private notes"
        [field, secure, empty, notes].forEach(w.addSubview)
        w.trooperUnmask = true // even unmasked, inputs stay masked
        [field, secure, empty, notes].forEach { $0.layoutIfNeeded() }

        let r = map(w, maskAll: false)
        XCTAssertEqual(node(r, field)?.kind, "input")
        XCTAssertEqual(node(r, field)?.text, String(repeating: "•", count: 14))
        XCTAssertEqual(node(r, secure)?.text, "•••••••")
        XCTAssertEqual(node(r, empty)?.text, "Email") // placeholder follows maskAllText
        XCTAssertEqual(node(r, notes)?.kind, "input")
        XCTAssertEqual(node(r, notes)?.masked, true)
        for leaked in ["me@example.com", "hunter2", "private notes"] {
            XCTAssertFalse(r.nodes.contains { $0.text == leaked }, "\(leaked) leaked")
        }
    }

    func testUnmaskIsInheritedAndCanBeForcedBack() throws {
        let w = window()
        let group = UIView(frame: CGRect(x: 0, y: 0, width: 390, height: 300))
        group.trooperUnmask = true
        let total = UILabel(frame: CGRect(x: 0, y: 0, width: 200, height: 20)); total.text = "Total: 42"
        let card = UILabel(frame: CGRect(x: 0, y: 30, width: 200, height: 20)); card.text = "Card 4242"
        card.trooperUnmask = false
        group.addSubview(total); group.addSubview(card)
        w.addSubview(group)
        let r = map(w)
        XCTAssertEqual(node(r, total)?.text, "Total: 42")
        XCTAssertEqual(node(r, total)?.masked, false)
        XCTAssertEqual(node(r, card)?.text, "•••• ••••")
        XCTAssertTrue(group.trooperUnmask)
    }

    func testHiddenTransparentAndClipped() throws {
        let w = window()
        let hidden = UILabel(frame: CGRect(x: 0, y: 0, width: 100, height: 20)); hidden.text = "x"; hidden.isHidden = true
        let clear = UILabel(frame: CGRect(x: 0, y: 0, width: 100, height: 20)); clear.text = "x"; clear.alpha = 0
        let half = UILabel(frame: CGRect(x: 0, y: 0, width: 100, height: 20)); half.text = "x"; half.alpha = 0.5
        let scroll = UIScrollView(frame: CGRect(x: 0, y: 100, width: 390, height: 100))
        scroll.contentSize = CGSize(width: 390, height: 300)
        scroll.contentOffset = CGPoint(x: 0, y: 50)
        let rows = (0..<3).map { i -> UILabel in
            let l = UILabel(frame: CGRect(x: 0, y: CGFloat(i) * 100, width: 390, height: 100)); l.text = "row \(i)"; return l
        }
        rows.forEach(scroll.addSubview)
        [hidden, clear, half, scroll].forEach(w.addSubview)
        let r = map(w)
        XCTAssertNil(node(r, hidden))
        XCTAssertNil(node(r, clear))
        XCTAssertEqual(node(r, half)?.alpha, 0.5)
        XCTAssertEqual(node(r, scroll)?.kind, "list")
        // Content offset 50: row 0 at y 50..150 → screen 50..150, visible from 100 (scroll top).
        let row0 = try XCTUnwrap(node(r, rows[0]))
        XCTAssertEqual(row0.y, 50)
        XCTAssertEqual(row0.clip, [0, 100, 390, 50])
        XCTAssertNotNil(node(r, rows[1])?.clip)
        XCTAssertNil(node(r, rows[2]), "row 2 (screen 250…350) is outside the scroll view")
    }

    func testWindowOriginIsApplied() throws {
        let w = UIWindow(frame: CGRect(x: 0, y: 400, width: 390, height: 444))
        w.isHidden = false
        let l = UILabel(frame: CGRect(x: 10, y: 10, width: 100, height: 20)); l.text = "sheet"
        w.addSubview(l)
        XCTAssertEqual(node(map(w), l)?.y, 410)
    }

    func testNodeBudget() {
        let w = window()
        for i in 0..<50 {
            let l = UILabel(frame: CGRect(x: 0, y: CGFloat(i) * 10, width: 100, height: 10)); l.text = "row"; w.addSubview(l)
        }
        let r = map(w, maxNodes: 20)
        XCTAssertTrue(r.truncated)
        XCTAssertEqual(r.nodes.count, 20)
    }

    /// Regression: a `.clear` container (e.g. UIKit's sheet dimming view) produced an empty `view` node.
    func testClearBackgroundIsNotDrawn() {
        let w = window()
        let clear = UIView(frame: CGRect(x: 0, y: 0, width: 100, height: 100))
        clear.backgroundColor = .clear
        let clearLayer = UIView(frame: CGRect(x: 0, y: 200, width: 100, height: 100))
        clearLayer.layer.backgroundColor = UIColor.clear.cgColor
        [clear, clearLayer].forEach(w.addSubview)
        let r = map(w)
        XCTAssertNil(node(r, clear))
        XCTAssertNil(node(r, clearLayer))
    }

    func testSystemWindowsAreExcludedByName() {
        XCTAssertTrue(UIKitWireframeMapper.isWebView(UIView()) == false)
        XCTAssertFalse(UIKitWireframeMapper.isHostingView(UIView()))
    }
}
#endif
