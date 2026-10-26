import XCTest
import TaskTrooperCrashC
@testable import TaskTrooper

final class ViewFilterTests: XCTestCase {
    func testContainersAndSystemControllersIgnored() {
        XCTAssertFalse(ViewNameFilter.shouldTrack(className: "UINavigationController", ancestors: [], isContainer: true, isAppCode: false))
        XCTAssertFalse(ViewNameFilter.shouldTrack(className: "MyApp.RootNav", ancestors: ["UINavigationController"], isContainer: false, isAppCode: true))
        XCTAssertFalse(ViewNameFilter.shouldTrack(className: "MyApp.Tabs", ancestors: [], isContainer: true, isAppCode: true))
        XCTAssertFalse(ViewNameFilter.shouldTrack(className: "_UIAlertControllerTextFieldViewController", ancestors: [], isContainer: false, isAppCode: true))
        XCTAssertFalse(ViewNameFilter.shouldTrack(className: "UIAlertController", ancestors: [], isContainer: false, isAppCode: false))
        XCTAssertFalse(ViewNameFilter.shouldTrack(className: "SFSafariViewController", ancestors: [], isContainer: false, isAppCode: false))
        XCTAssertFalse(ViewNameFilter.shouldTrack(className: "_TtGC7SwiftUI19UIHostingControllerV5MyApp8RootView_", ancestors: [], isContainer: false, isAppCode: false))
        XCTAssertFalse(ViewNameFilter.shouldTrack(className: "MyApp.Host", ancestors: ["_TtGC7SwiftUI19UIHostingControllerV5MyApp8RootView_"], isContainer: false, isAppCode: true))
    }

    func testAppControllersTracked() {
        XCTAssertTrue(ViewNameFilter.shouldTrack(className: "MyApp.CheckoutViewController", ancestors: ["MyApp.BaseViewController"], isContainer: false, isAppCode: true))
        XCTAssertEqual(ViewNameFilter.displayName("MyApp.CheckoutViewController"), "CheckoutViewController")
        XCTAssertEqual(ViewNameFilter.displayName("LegacyObjCController"), "LegacyObjCController")
        XCTAssertEqual(ViewNameFilter.displayName("ListController<Product>"), "ListController<Product>")
    }

    func testIgnoredList() {
        XCTAssertFalse(ViewNameFilter.shouldTrack(className: "MyApp.Splash", ancestors: [], isContainer: false, isAppCode: true, ignored: ["Splash"]))
        XCTAssertFalse(ViewNameFilter.shouldTrack(className: "MyApp.Splash", ancestors: [], isContainer: false, isAppCode: true, ignored: ["MyApp.Splash"]))
    }
}

final class ViewStackTests: XCTestCase {
    private var n = 0
    private func id() -> String { n += 1; return "id\(n)" }

    func testPushAndPop() {
        var s = ViewStack()
        XCTAssertNotNil(s.start(key: "A", name: "A", id: id(), ts: 0).started)
        let push = s.start(key: "B", name: "B", id: id(), ts: 10)
        XCTAssertEqual(push.ended?.name, "A")
        XCTAssertEqual(push.started?.name, "B")
        // A.viewDidDisappear arrives after B appeared: A is not current, nothing emitted.
        let aGone = s.stop(key: "A", ts: 11, newId: id)
        XCTAssertNil(aGone.ended)
        XCTAssertEqual(s.current?.name, "B")
    }

    func testDismissedSheetResumesPresenter() {
        var s = ViewStack()
        _ = s.start(key: "A", name: "A", id: "a1", ts: 0)
        _ = s.start(key: "S", name: "Sheet", id: "s1", ts: 5)
        let r = s.stop(key: "S", ts: 9, newId: { "a2" })
        XCTAssertEqual(r.ended?.name, "Sheet")
        XCTAssertEqual(r.resumed?.id, "a2")
        XCTAssertEqual(r.resumed?.startMs, 9)
        // Presenter's viewDidAppear (if delivered) does not create a duplicate view.
        let again = s.start(key: "A", name: "A", id: "a3", ts: 10)
        XCTAssertNil(again.started)
        XCTAssertNil(again.ended)
    }

    func testBackgroundSuspendResume() {
        var s = ViewStack()
        _ = s.start(key: "A", name: "A", id: "a1", ts: 0)
        XCTAssertEqual(s.suspend()?.id, "a1")
        XCTAssertNil(s.current)
        let resumed = s.resume(ts: 100, newId: { "a2" })
        XCTAssertEqual(resumed?.id, "a2")
        XCTAssertEqual(s.current?.id, "a2")
    }
}

final class VitalsLogicTests: XCTestCase {
    func testFrameClassification() {
        var f = FrameStats()
        let e = 1.0 / 60
        for _ in 0..<90 { f.record(duration: e) }                     // on budget (incl. vsync jitter)
        for _ in 0..<8 { f.record(duration: 0.05) }                   // slow
        f.record(duration: 0.8)                                        // frozen
        f.record(duration: 120)                                        // suspension gap: ignored
        f.record(duration: 1.0 / 120)                                  // 120 Hz frame: fine
        XCTAssertEqual(f.total, 100)
        XCTAssertEqual(f.slow, 8)
        XCTAssertEqual(f.frozen, 1)
        let vitals = f.vitals()
        XCTAssertEqual(vitals.map(\.name), ["SLOW_FRAMES", "FROZEN_FRAMES"])
        XCTAssertEqual(vitals.map(\.value), [8, 1])
        XCTAssertTrue(FrameStats().vitals().isEmpty)
    }

    func testHangState() {
        var h = HangState(threshold: 2)
        XCTAssertTrue(h.shouldPing(now: 0))
        XCTAssertFalse(h.shouldPing(now: 0.25), "one ping outstanding")
        XCTAssertNil(h.pong(now: 0.1))
        XCTAssertTrue(h.shouldPing(now: 1))
        XCTAssertEqual(h.pong(now: 3.5), 2.5)
        XCTAssertNil(h.pong(now: 4), "already answered")
        XCTAssertTrue(h.shouldPing(now: 5))
        h.reset()
        XCTAssertNil(h.pong(now: 100), "reset discards pending ping (background)")
    }

    func testHangDetectorReportsBlockedMainThread() {
        let exp = expectation(description: "hang")
        let reported = Locked<TimeInterval?>(nil)
        let stack = Locked<[UInt64]?>(nil)
        let d = HangDetector(threshold: 0.5) { duration, frames in
            reported.set(duration)
            stack.set(frames)
            exp.fulfill()
        }
        d.start()
        // Let the watchdog post a ping, then block the main thread.
        RunLoop.main.run(until: Date().addingTimeInterval(0.4))
        Thread.sleep(forTimeInterval: 1.0)
        wait(for: [exp], timeout: 5)
        d.pause()
        XCTAssertGreaterThanOrEqual(reported.current ?? 0, 0.5)
        // The main thread stack was sampled while it was blocked (inside Thread.sleep).
        let frames = stack.current ?? []
        XCTAssertGreaterThan(frames.count, 2, "main thread stack captured during the hang")
        let text = HangDetector.stackText(frames)
        XCTAssertTrue(text.hasPrefix("Thread 0 name:  main\nThread 0:\n"), text)
        XCTAssertTrue(text.contains("Binary Images:"))
        XCTAssertTrue(text.contains("libsystem_kernel.dylib"), "blocked in a syscall: \(text)")
    }

    func testHangStateCapturesOncePerPing() {
        var h = HangState(threshold: 2)
        XCTAssertFalse(h.shouldCapture(now: 0), "nothing pending")
        XCTAssertTrue(h.shouldPing(now: 0))
        XCTAssertFalse(h.shouldCapture(now: 1.9))
        XCTAssertTrue(h.shouldCapture(now: 2.1))
        XCTAssertFalse(h.shouldCapture(now: 3), "one sample per hang")
        XCTAssertEqual(h.pong(now: 4), 4)
        XCTAssertTrue(h.shouldPing(now: 5))
        XCTAssertTrue(h.shouldCapture(now: 7.5), "re-armed for the next hang")
    }

    func testSampleBlockedBackgroundThread() {
        // Capture another (blocked) thread's stack the same way the watchdog samples the main thread.
        let ready = DispatchSemaphore(value: 0)
        let release = DispatchSemaphore(value: 0)
        let port = Locked<mach_port_t>(0)
        let t = Thread {
            port.set(pthread_mach_thread_np(pthread_self()))
            ready.signal()
            release.wait()
        }
        t.start()
        ready.wait()
        Thread.sleep(forTimeInterval: 0.05)
        var buf = [UInt](repeating: 0, count: 64)
        let n = buf.withUnsafeMutableBufferPointer { tt_capture_thread_stack(port.current, $0.baseAddress, 64) }
        release.signal()
        XCTAssertGreaterThan(n, 2)
        XCTAssertEqual(tt_capture_thread_stack(pthread_mach_thread_np(pthread_self()), &buf, 64), 0, "own thread is refused")
    }
}
