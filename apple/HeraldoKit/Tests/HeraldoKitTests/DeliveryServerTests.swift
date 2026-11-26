import XCTest
@testable import HeraldoKit

/// Delivery settings against a real heraldo-server. `apple/scripts/test-server.sh`
/// starts one and sets the variables, the admin token included; without them
/// these skip. The test turns `delivery.enabled` on for its own run and off
/// again, and its row matches only its own source.
final class DeliveryServerTests: XCTestCase {
    var client: HeraldoClient!

    override func setUpWithError() throws {
        let env = ProcessInfo.processInfo.environment
        guard let url = env["HERALDO_TEST_URL"].flatMap(URL.init(string:)),
              let read = env["HERALDO_TEST_READ_TOKEN"], let write = env["HERALDO_TEST_WRITE_TOKEN"],
              let admin = env["HERALDO_TEST_ADMIN_TOKEN"] else {
            throw XCTSkip("no test server with an admin token; run apple/scripts/test-server.sh")
        }
        client = HeraldoClient(baseURL: url, readToken: read, writeToken: write, adminToken: admin)
    }

    func testRowsStampHiddenAndSources() async throws {
        let source = "dlv-\(UUID().uuidString.prefix(8).lowercased())"
        let c = client!
        try await c.post(EventInput(source: source, kind: "message", title: "Before", level: "active"))

        try await c.putSetting("delivery.enabled", .bool(true))
        addTeardownBlock { _ = try? await c.putSetting("delivery.enabled", .bool(false)) }
        let list = try await XCTUnwrapAsync(await c.deliverySettings())
        XCTAssertTrue(list.enabled)

        let row = try await c.createDeliverySetting(DeliverySettingInput(
            match: DeliveryMatch(source: source.uppercased()), deliver: .drop, position: 9000, note: "Test drop"))
        addTeardownBlock { try? await c.deleteDeliverySetting(id: row.id) }
        XCTAssertEqual(row.deliver, .drop)
        XCTAssertEqual(row.badge, .auto)
        XCTAssertEqual(row.note, "Test drop")

        // A row the server refuses comes back as a 400 with its reason.
        do {
            try await c.createDeliverySetting(DeliverySettingInput(deliver: .drop, badge: .on))
            XCTFail("the server stored badge on with drop")
        } catch let e as HeraldoError {
            XCTAssertEqual(e.status, 400)
        }

        let dropped = try await c.post(EventInput(source: source, kind: "message", title: "Dropped", level: "active"))
        XCTAssertEqual(dropped.deliver?.outcome, .drop)
        XCTAssertEqual(dropped.deliver?.by, row.id)
        XCTAssertEqual(dropped.deliver?.levelWas, "active")
        XCTAssertTrue(dropped.isHidden)

        let shown = try await c.listEvents(EventQuery(source: source, hidden: .exclude)).events
        XCTAssertEqual(shown.map(\.title), ["Before"])
        let filed = try await c.listEvents(EventQuery(source: source, hidden: .only)).events
        XCTAssertEqual(filed.map(\.id), [dropped.id])
        let both = try await c.listEvents(EventQuery(source: source)).events
        XCTAssertEqual(both.count, 2)

        // The preview runs the given rows over this source's two events.
        let preview = try await c.previewDelivery(DeliveryPreviewInput(settings: [DeliverySettingInput(row)], days: 1))
        XCTAssertEqual(preview.row(0)?.hits, 2)
        XCTAssertTrue(preview.changed.contains { $0.id == dropped.id && $0.applied == "drop" })

        let sources = try await XCTUnwrapAsync(await c.sources())
        let mine = try XCTUnwrap(sources.first { $0.source == source })
        XCTAssertTrue(mine.new)
        XCTAssertEqual(mine.events, 2)
        XCTAssertEqual(mine.badgeIn + mine.badgeOut, 2)
        let reviewed = try await c.reviewSource(source)
        XCTAssertFalse(reviewed.new)
        XCTAssertNotNil(reviewed.reviewedAt)

        var edit = DeliverySettingInput(row)
        edit.deliver = .silent
        edit.badge = .off
        let updated = try await c.updateDeliverySetting(id: row.id, edit)
        XCTAssertEqual(updated.deliver, .silent)
        XCTAssertEqual(updated.badge, .off)
        XCTAssertEqual(updated.note, "Test drop", "PUT kept the note")
        let fetched = try await c.deliverySetting(id: row.id)
        XCTAssertEqual(fetched.position, 9000)

        try await c.deleteDeliverySetting(id: row.id)
        let after = try await XCTUnwrapAsync(await c.deliverySettings())
        XCTAssertFalse(after.settings.contains { $0.id == row.id })
    }

    func testReadTokenCannotWriteRows() async throws {
        var readOnly = client!
        readOnly.adminToken = readOnly.readToken
        do {
            try await readOnly.createDeliverySetting(DeliverySettingInput(deliver: .silent))
            XCTFail("a read token wrote a row")
        } catch let e as HeraldoError {
            XCTAssertEqual(e.status, 403)
        }
    }

    private func XCTUnwrapAsync<T>(_ value: T?, file: StaticString = #filePath, line: UInt = #line) async throws -> T {
        try XCTUnwrap(value, file: file, line: line)
    }
}
