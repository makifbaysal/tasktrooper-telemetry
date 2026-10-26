import XCTest
@testable import TaskTrooper

final class SessionTests: XCTestCase {
    let minute: Int64 = 60_000

    func testRotatesAfterInactivity() {
        let s = SessionManager(defaults: freshDefaults(), sampleRate: 1)
        let t0: Int64 = 1_000_000
        let a = s.touch(nowMs: t0)
        XCTAssertTrue(a.isNew)
        let b = s.touch(nowMs: t0 + 29 * minute)
        XCTAssertFalse(b.isNew)
        XCTAssertEqual(a.session.id, b.session.id)
        let c = s.touch(nowMs: t0 + 29 * minute + 30 * minute)
        XCTAssertTrue(c.isNew)
        XCTAssertNotEqual(c.session.id, a.session.id)
    }

    func testRotatesAfterFourHoursEvenWhenActive() {
        let s = SessionManager(defaults: freshDefaults(), sampleRate: 1)
        let t0: Int64 = 5_000_000
        let first = s.touch(nowMs: t0).session.id
        var t = t0
        while t < t0 + 4 * 60 * minute - 10 * minute {
            t += 10 * minute
            XCTAssertEqual(s.touch(nowMs: t).session.id, first)
        }
        let rotated = s.touch(nowMs: t0 + 4 * 60 * minute)
        XCTAssertTrue(rotated.isNew)
    }

    func testPersistsAcrossInstances() {
        let d = freshDefaults()
        let a = SessionManager(defaults: d, sampleRate: 1).touch(nowMs: 1000).session.id
        let b = SessionManager(defaults: d, sampleRate: 1).touch(nowMs: 2000)
        XCTAssertFalse(b.isNew)
        XCTAssertEqual(a, b.session.id)
    }

    func testSamplingDecidedPerSession() {
        XCTAssertFalse(SessionManager(defaults: freshDefaults(), sampleRate: 0.5, random: { 0.7 }).touch(nowMs: 1).session.sampled)
        XCTAssertTrue(SessionManager(defaults: freshDefaults(), sampleRate: 0.5, random: { 0.2 }).touch(nowMs: 1).session.sampled)
        XCTAssertFalse(SessionManager(defaults: freshDefaults(), sampleRate: 0, random: { 0 }).touch(nowMs: 1).session.sampled)
    }

    func testAnonymousIdIsStable() {
        let d = freshDefaults()
        XCTAssertEqual(AnonymousID.load(d), AnonymousID.load(d))
    }
}
