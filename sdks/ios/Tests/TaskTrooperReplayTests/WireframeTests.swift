import XCTest
@_spi(TrooperReplay) import TaskTrooper
@testable import TaskTrooperReplay

final class WireframeTests: XCTestCase {
    func testMaskKeepsLengthAndWhitespace() {
        XCTAssertEqual(Masking.mask("John Doe"), "•••• •••")
        XCTAssertEqual(Masking.mask("ab\nc"), "••\n•")
        XCTAssertEqual(Masking.mask(String(repeating: "x", count: 1000)).count, Masking.maxText)
    }

    func testMaskingRules() {
        XCTAssertEqual(Masking.apply("abc", input: false, secure: false, maskAll: true, unmask: false).text, "•••")
        XCTAssertEqual(Masking.apply("abc", input: false, secure: false, maskAll: true, unmask: true).text, "abc")
        XCTAssertEqual(Masking.apply("abc", input: false, secure: false, maskAll: false, unmask: false).text, "abc")
        XCTAssertEqual(Masking.apply("abc", input: true, secure: false, maskAll: false, unmask: true).text, "•••")
        XCTAssertEqual(Masking.apply("abc", input: false, secure: true, maskAll: false, unmask: true).text, "•••")
        XCTAssertTrue(Masking.apply("abc", input: true, secure: false, maskAll: false, unmask: true).masked)
        XCTAssertNil(Masking.apply("", input: true, secure: false, maskAll: true, unmask: false).text)
    }

    func testColors() {
        XCTAssertEqual(WireColor.hex(0xff0000ff), "#ff0000")
        XCTAssertEqual(WireColor.hex(0x00ff0080), "#00ff0080")
        XCTAssertEqual(WireColor.rgba(1, 0, 0, 1), 0xff0000ff)
        XCTAssertEqual(WireColor.rgba(0, 0, 1, 0.5), 0x0000ff80)
        XCTAssertTrue(WireColor.isTransparent(0xffffff00))
    }

    func testNodeJSON() throws {
        var j = JSONText()
        WireNode(id: 7, x: 1.04, y: 2, w: 3.333, h: 4, kind: "button", bg: 0x336699ff, fg: 0x00000080, radius: 8, text: "••",
                 masked: true, alpha: 0.5, clip: [1, 2, 3, 2]).write(&j)
        let o = try XCTUnwrap(JSONSerialization.jsonObject(with: Data(j.text.utf8)) as? [String: Any])
        XCTAssertEqual(o["id"] as? Int, 7)
        XCTAssertEqual(o["x"] as? Double, 1)
        XCTAssertEqual(o["w"] as? Double, 3.3)
        XCTAssertEqual(o["bg"] as? String, "#336699")
        XCTAssertEqual(o["fg"] as? String, "#000000")
        XCTAssertEqual(o["radius"] as? Int, 8)
        XCTAssertEqual(o["masked"] as? Bool, true)
        XCTAssertEqual(o["alpha"] as? Double, 0.5)
        XCTAssertEqual((o["clip"] as? [Any])?.count, 4)

        var p = JSONText()
        WireNode(id: 1, x: 0, y: 0, w: 1, h: 1, kind: "view").write(&p)
        let plain = try XCTUnwrap(JSONSerialization.jsonObject(with: Data(p.text.utf8)) as? [String: Any])
        XCTAssertEqual(Set(plain.keys), ["id", "x", "y", "w", "h", "kind"])
    }

    func testStringEscaping() throws {
        var j = JSONText()
        j.beginObject(); j.key("t"); j.string("a\"b\\c\n\u{1}"); j.endObject()
        let o = try XCTUnwrap(JSONSerialization.jsonObject(with: Data(j.text.utf8)) as? [String: String])
        XCTAssertEqual(o["t"], "a\"b\\c\n\u{1}")
    }

    func testSegmentStoreDropsBeyondCap() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("tt-replay-\(UUID().uuidString)")
        let store = try XCTUnwrap(SegmentStore(directory: dir, maxBytes: 100))
        XCTAssertTrue(store.write(Data(count: 60), nowMs: 1))
        XCTAssertFalse(store.write(Data(count: 60), nowMs: 2))
        XCTAssertEqual(store.files().count, 1)
        XCTAssertEqual(store.dropped, 1)
    }

    func testOptionsDefaults() {
        let o = TrooperReplayOptions()
        XCTAssertEqual(o.sampleRate, 0.1)
        XCTAssertEqual(o.onErrorSampleRate, 1.0)
        XCTAssertTrue(o.maskAllText)
        XCTAssertTrue(o.enabled)
    }
}
