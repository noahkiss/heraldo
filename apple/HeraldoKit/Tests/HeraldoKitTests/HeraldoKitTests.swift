import XCTest
@testable import HeraldoKit

final class AppInfoTests: XCTestCase {
    func testDisplayNameFallsBack() {
        XCTAssertFalse(AppInfo.displayName(Bundle(for: Self.self)).isEmpty)
    }
}
