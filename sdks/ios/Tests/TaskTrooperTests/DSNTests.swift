import XCTest
@testable import TaskTrooper

final class DSNTests: XCTestCase {
    func testParsesFullDSN() throws {
        let dsn = try XCTUnwrap(TrooperDSN("https://abc123@ingest.trooper.dev:8443/42"))
        XCTAssertEqual(dsn.key, "abc123")
        XCTAssertEqual(dsn.projectId, "42")
        XCTAssertEqual(dsn.baseURL.absoluteString, "https://ingest.trooper.dev:8443")
        XCTAssertEqual(dsn.rumURL.absoluteString, "https://ingest.trooper.dev:8443/v1/rum")
    }

    func testParsesLocalDevDSN() throws {
        let dsn = try XCTUnwrap(TrooperDSN("http://tt_dev_ingest_key@localhost:4318/1"))
        XCTAssertEqual(dsn.key, "tt_dev_ingest_key")
        XCTAssertEqual(dsn.projectId, "1")
        XCTAssertEqual(dsn.rumURL.absoluteString, "http://localhost:4318/v1/rum")
    }

    func testPathPrefixIsKept() throws {
        let dsn = try XCTUnwrap(TrooperDSN("https://k@example.com/ingest/7/"))
        XCTAssertEqual(dsn.projectId, "7")
        XCTAssertEqual(dsn.rumURL.absoluteString, "https://example.com/ingest/v1/rum")
    }

    func testRejectsInvalid() {
        for s in ["", "not a url", "https://example.com/1", "https://k@example.com", "https://k@example.com/",
                  "ftp://k@example.com/1", "https://k@/1"] {
            XCTAssertNil(TrooperDSN(s), s)
        }
    }
}
