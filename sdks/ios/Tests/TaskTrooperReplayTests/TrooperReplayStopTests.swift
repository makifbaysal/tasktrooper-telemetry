#if canImport(UIKit) && os(iOS)
import XCTest
@testable import TaskTrooperReplay

@MainActor
final class TrooperReplayStopTests: XCTestCase {
    private func stopAndWait() {
        let exp = expectation(description: "stop")
        TrooperReplay.stop { exp.fulfill() }
        wait(for: [exp], timeout: 5)
    }

    func testStopIsIdempotentAndInstallWorksAgain() {
        stopAndWait() // clean slate (another test may have installed)
        TrooperReplay.install(TrooperReplayOptions(sampleRate: 1))
        let first = TrooperReplay.controller.current
        XCTAssertNotNil(first)
        XCTAssertNotNil(ReplayRecorder.shared)

        stopAndWait()
        XCTAssertNil(TrooperReplay.controller.current)
        XCTAssertNil(ReplayRecorder.shared, "captures, keyboard and touch observation end")
        XCTAssertFalse(TrooperReplay.isRecording)
        XCTAssertFalse(first!.isCapturing.current)
        stopAndWait() // second stop: no-op, completion still called

        TrooperReplay.install(TrooperReplayOptions(sampleRate: 1))
        let second = TrooperReplay.controller.current
        XCTAssertNotNil(second)
        XCTAssertFalse(second === first)
        XCTAssertNotNil(ReplayRecorder.shared)
        stopAndWait()
    }

    func testStoppedControllerIgnoresInput() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let c = try XCTUnwrap(ReplayController(options: TrooperReplayOptions(sampleRate: 1), defaults: UserDefaults(suiteName: UUID().uuidString)!,
                                               directory: dir))
        let exp = expectation(description: "stop")
        c.stop { exp.fulfill() }
        wait(for: [exp], timeout: 5)
        c.frame(ts: 1, width: 390, height: 844, density: 3, nodes: [])
        c.touch(ts: 2, x: 1, y: 1, phase: "down")
        let done = expectation(description: "refresh")
        c.refreshSession { capturing in XCTAssertFalse(capturing); done.fulfill() }
        wait(for: [done], timeout: 5)
        XCTAssertFalse(c.isCapturing.current)
        XCTAssertFalse(c.isRecording.current)
    }
}
#endif
