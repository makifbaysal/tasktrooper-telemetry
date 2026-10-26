import XCTest
import TaskTrooperCrashC
@testable import TaskTrooper

final class CrashReportTests: XCTestCase {
    private var url: URL!

    override func setUp() {
        url = tempDirectory().appendingPathComponent("crash.report")
        XCTAssertTrue(CrashReporter.install(at: url, signals: false, exceptions: false))
        tt_crash_reset_for_test()
    }

    override func tearDown() {
        tt_crash_uninstall()
    }

    func testSignalReportRoundTrip() throws {
        CrashReporter.setContext(sessionId: "sess-1", viewId: "view-1", viewName: "Checkout", userId: "user-9")
        XCTAssertEqual(tt_crash_write_test_report(SIGSEGV, 0x10), 0)

        let report = try XCTUnwrap(CrashReporter.loadPending(at: url))
        XCTAssertFalse(FileManager.default.fileExists(atPath: url.path), "pending report consumed")
        XCTAssertTrue(report.complete)
        XCTAssertEqual(report.signal, SIGSEGV)
        XCTAssertEqual(report.signalName, "SIGSEGV")
        XCTAssertEqual(report.faultAddress, 0x10)
        XCTAssertEqual(report.sessionId, "sess-1")
        XCTAssertEqual(report.viewId, "view-1")
        XCTAssertEqual(report.viewName, "Checkout")
        XCTAssertEqual(report.userId, "user-9")
        XCTAssertEqual(report.thread, "main")
        XCTAssertGreaterThan(report.tsMs, 1_600_000_000_000)
        XCTAssertGreaterThan(report.frames.count, 1)
        XCTAssertFalse(report.images.isEmpty, "binary images for frames")
        let first = report.frames[0]
        XCTAssertTrue(report.images.contains { first >= $0.loadAddress && first < $0.loadAddress + $0.size })
        XCTAssertTrue(report.images.allSatisfy { $0.uuid.count == 36 })

        let payload = report.errorPayload()
        XCTAssertEqual(payload.type, "SIGSEGV")
        XCTAssertEqual(payload.source, "crash")
        XCTAssertEqual(payload.isCrash, true)
        XCTAssertTrue(payload.message.contains("0x0000000000000010"))
        XCTAssertTrue(payload.stack?.contains("Binary Images:") ?? false)
        XCTAssertTrue(payload.stack?.contains("Thread 0 Crashed:") ?? false)
    }

    func testExceptionReport() throws {
        CrashReporter.setContext(sessionId: "s2", viewId: nil, viewName: nil, userId: nil)
        let exc = NSException(name: .invalidArgumentException, reason: "bad index\nsecond line", userInfo: nil)
        XCTAssertEqual(tt_crash_write_exception_report(exc), 0)
        XCTAssertEqual(tt_crash_report_written(), 1)
        let report = try XCTUnwrap(CrashReporter.loadPending(at: url))
        XCTAssertEqual(report.exceptionName, "NSInvalidArgumentException")
        XCTAssertEqual(report.exceptionReason, "bad index\nsecond line")
        XCTAssertEqual(report.signalName, "SIGABRT")
        XCTAssertEqual(report.errorPayload().type, "NSInvalidArgumentException")
        XCTAssertEqual(report.errorPayload().message, "bad index\nsecond line")
    }

    func testAllThreadsAndCrashInfoMessage() throws {
        let ready = DispatchSemaphore(value: 0)
        let release = DispatchSemaphore(value: 0)
        let t = Thread {
            ready.signal()
            release.wait()
        }
        t.start()
        ready.wait()
        tt_crash_set_annotation("Fatal error: Index out of range\nfile Shop/Cart.swift, line 42")
        defer { tt_crash_set_annotation(nil); release.signal() }

        XCTAssertEqual(tt_crash_write_test_report(SIGTRAP, 0), 0)
        let report = try XCTUnwrap(CrashReporter.loadPending(at: url))
        XCTAssertTrue(report.complete)
        XCTAssertGreaterThanOrEqual(report.threads.count, 1, "other threads captured")
        XCTAssertTrue(report.threads.contains { $0.frames.count > 1 }, "blocked thread has a stack")
        XCTAssertFalse(report.threads.contains { $0.name == "main" }, "crashed thread (main) is not repeated")
        XCTAssertEqual(report.crashInfoMessage, "Fatal error: Index out of range\nfile Shop/Cart.swift, line 42")

        let payload = report.errorPayload()
        XCTAssertEqual(payload.type, "SIGTRAP")
        XCTAssertEqual(payload.message, "Fatal error: Index out of range\nfile Shop/Cart.swift, line 42")
        let stack = try XCTUnwrap(payload.stack)
        XCTAssertTrue(stack.hasPrefix("Thread 0 name:  main\nThread 0 Crashed:\n"), stack)
        XCTAssertTrue(stack.contains("\nThread 1:\n"))
        // Every frame that lies in a binary image references an image listed under Binary Images.
        let imagesSection = try XCTUnwrap(stack.components(separatedBy: "Binary Images:\n").last)
        for t in report.threads { for f in t.frames {
            if let img = report.images.first(where: { f >= $0.loadAddress && f < $0.loadAddress + $0.size }) {
                XCTAssertTrue(imagesSection.contains(img.uuid))
            }
        } }
    }

    func testParsesThreadsAndPrefersSwiftCrashInfo() throws {
        let text = """
        TTCRASH 1
        ts 1700000000000
        signal 5 SIGTRAP
        code 1
        addr 0x0000000000000000
        session s
        view_id\u{20}
        view_name\u{20}
        user\u{20}
        crash_info libsystem_sim_platform.dylib: CoreSimulator 1174 - Device: iPhone 17 Pro
        crash_info libsystem_c.dylib: abort() called
        crash_info libswiftCore.dylib: Fatal error: boom\\nsecond
        thread main
        frame 0x0000000100001000
        thread_begin background
        tframe 0x0000000100002000
        tframe 0x0000000100003000
        thread_begin background
        image 0x0000000100000000 0x0000000000010000 0123456789abcdef0123456789abcdef /app/Shop
        end
        """
        let r = try XCTUnwrap(CrashReport.parse(text))
        XCTAssertEqual(r.threads.count, 2)
        XCTAssertEqual(r.threads[0].frames, [0x100002000, 0x100003000])
        XCTAssertEqual(r.threads[1].frames, [])
        XCTAssertEqual(r.crashInfo.count, 3)
        XCTAssertEqual(r.crashInfoMessage, "Fatal error: boom\nsecond")
        XCTAssertEqual(r.message, "Fatal error: boom\nsecond")
        let stack = r.stackText()
        XCTAssertTrue(stack.contains("Thread 2:\n"))
        XCTAssertTrue(stack.contains("1   Shop                           0x0000000100003000 0x100000000 + 12288"), stack)
        XCTAssertTrue(stack.contains("<01234567-89AB-CDEF-0123-456789ABCDEF> /app/Shop"))
    }

    func testParserRejectsGarbageAndHandlesPartialReports() {
        XCTAssertNil(CrashReport.parse("hello"))
        let partial = CrashReport.parse("TTCRASH 1\nts 1700000000000\nsignal 6 SIGABRT\nframe 0x10")
        XCTAssertEqual(partial?.complete, false)
        XCTAssertEqual(partial?.frames, [0x10])
    }

    func testEmptyFileIsNotAReport() {
        XCTAssertNil(CrashReporter.loadPending(at: url), "install truncates; nothing written")
    }

    func testProcessInfoHelpers() {
        let start = tt_process_start_time_ms()
        let now = Int64(Date().timeIntervalSince1970 * 1000)
        XCTAssertGreaterThan(start, 0)
        XCTAssertLessThanOrEqual(start, now)
    }
}
