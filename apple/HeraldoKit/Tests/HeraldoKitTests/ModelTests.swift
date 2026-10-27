import XCTest
@testable import HeraldoKit

/// Decoding and helpers, from hand-written fixtures.
final class ModelTests: XCTestCase {
    func testDecodesStoredEvent() throws {
        let json = """
        {
          "events": [{
            "id": "01JBA6Y8Q2K9M3T4V5W6X7Y8Z9",
            "received_at": "2026-10-26T12:00:01.123456Z",
            "ts": "2026-10-26T12:00:00Z",
            "source": "uptime",
            "kind": "incident",
            "key": "unit:web-1:nginx.service",
            "action": "open",
            "title": "Web is down",
            "level": "critical",
            "actions": [{"id": "ack", "title": "Acknowledge", "type": "server"}],
            "hold": {"state": "open"},
            "delivery": [{"device": "dev_01", "outcome": "sent", "level": "time-sensitive", "at": "2026-10-26T12:00:02Z"}],
            "details": {"extra": {"n": 1}}
          }],
          "next_cursor": "s2"
        }
        """
        let page = try JSON.decoder.decode(EventPage.self, from: Data(json.utf8))
        XCTAssertEqual(page.nextCursor, "s2")
        let e = try XCTUnwrap(page.events.first)
        XCTAssertEqual(e.headline, "Web is down")
        XCTAssertTrue(e.isOpenIncident)
        XCTAssertEqual(e.delivery?.first?.outcome, "sent")
        XCTAssertNotNil(e.receivedAt)
        XCTAssertEqual(e.date, JSON.parseDate("2026-10-26T12:00:00Z"))
    }

    func testEmptyPageHasNoCursor() throws {
        let page = try JSON.decoder.decode(EventPage.self, from: Data(#"{"events": []}"#.utf8))
        XCTAssertTrue(page.events.isEmpty)
        XCTAssertNil(page.nextCursor)
    }

    func testRegistrationEncodesSnakeCase() throws {
        let r = DeviceRegistration(token: "ab12", environment: .production, topic: "com.example.app",
                                   name: "Phone", platform: "iOS", appVersion: "1.0 (1)",
                                   permissions: Permissions(alerts: true, timeSensitive: false))
        let obj = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(r)) as? [String: Any])
        XCTAssertEqual(obj["environment"] as? String, "production")
        XCTAssertEqual(obj["app_version"] as? String, "1.0 (1)")
        let p = try XCTUnwrap(obj["permissions"] as? [String: Any])
        XCTAssertEqual(p["time_sensitive"] as? Bool, false)
        XCTAssertNil(p["live_activities"], "an unknown permission is left out")
    }

    func testPushInfo() {
        let info = PushInfo(userInfo: [
            "aps": ["category": "incident"],
            "heraldo": ["id": "01ABC", "kind": "incident", "source": "uptime", "key": "k", "truncated": true],
        ])
        XCTAssertEqual(info?.id, "01ABC")
        XCTAssertEqual(info?.truncated, true)
        XCTAssertNil(PushInfo(userInfo: ["aps": [:]]))
    }

    func testAPNsEnvironment() {
        func profile(_ env: String?, key: String = "aps-environment") -> Data {
            let ent = env.map { "<key>\(key)</key><string>\($0)</string>" } ?? ""
            let plist = """
            <?xml version="1.0" encoding="UTF-8"?>
            <plist version="1.0"><dict><key>Entitlements</key><dict>\(ent)</dict></dict></plist>
            """
            // A CMS envelope wraps the plist; binary bytes on both sides.
            return Data([0x30, 0x82, 0x01]) + Data(plist.utf8) + Data([0x00, 0xA0])
        }
        XCTAssertEqual(APNs.environment(profile: profile("development"), simulator: false), .sandbox)
        XCTAssertEqual(APNs.environment(profile: profile("production"), simulator: false), .production)
        XCTAssertEqual(APNs.environment(profile: profile("production", key: "com.apple.developer.aps-environment"),
                                        simulator: false), .production, "a macOS profile")
        XCTAssertEqual(APNs.environment(profile: profile("development", key: "com.apple.developer.aps-environment"),
                                        simulator: false), .sandbox)
        XCTAssertEqual(APNs.environment(profile: nil, simulator: false), .production, "TestFlight has no profile")
        XCTAssertEqual(APNs.environment(profile: nil, simulator: true), .sandbox)
        XCTAssertEqual(APNs.hex(Data([0x0a, 0xff])), "0aff")
    }

    func testFailingCheckin() throws {
        func checkin(_ action: String, _ exit: Int? = nil) throws -> Event {
            let code = exit.map { #","checkin":{"rid":"r1","exit_code":\#($0)}"# } ?? ""
            let json = #"{"id":"01","source":"cron","kind":"checkin","key":"backup","action":"\#(action)"\#(code)}"#
            return try JSON.decoder.decode(Event.self, from: Data(json.utf8))
        }
        XCTAssertTrue(try checkin("fail").isFailingCheckin)
        XCTAssertTrue(try checkin("success", 2).isFailingCheckin)
        XCTAssertEqual(try checkin("success", 2).checkin?.exitCode, 2)
        XCTAssertFalse(try checkin("success", 0).isFailingCheckin)
        XCTAssertFalse(try checkin("start").isFailingCheckin)
        XCTAssertFalse(try checkin("log", 1).isFailingCheckin)
        XCTAssertFalse(Event(id: "x", source: "s", kind: "incident").isFailingCheckin)
    }

    func testCacheMergeDropsCopies() {
        var c = HistoryCache()
        c.prepend([Event(id: "b", source: "s"), Event(id: "a", source: "s")])
        c.prepend([Event(id: "c", source: "t"), Event(id: "b", source: "s")])
        c.append([Event(id: "a", source: "s"), Event(id: "0", source: "s")])
        XCTAssertEqual(c.events.map(\.id), ["c", "b", "a", "0"])
        XCTAssertEqual(c.sources, ["s", "t"])
    }

    func testMarkdownBlocks() {
        let body = """
        # Disk full
        The **root** volume
        is at 98%.

        - one
        * two
        1. first
        > quoted
        ---
        ```
        df -h
        ```
        """
        XCTAssertEqual(Markdown.blocks(body), [
            .heading(level: 1, text: "Disk full"),
            .paragraph("The **root** volume\nis at 98%."),
            .bullet("one"),
            .bullet("two"),
            .numbered(1, "first"),
            .quote("quoted"),
            .rule,
            .code("df -h"),
        ])
        XCTAssertEqual(Markdown.plainText("Hello **bold** [link](https://example.com)\n\n- item"),
                       "Hello bold link\n• item")
    }

    func testClientRefusesBadSettings() {
        XCTAssertThrowsError(try HeraldoClient(credentials: Credentials(serverURL: "", readToken: "t")))
        XCTAssertThrowsError(try HeraldoClient(credentials: Credentials(serverURL: "ftp://x", readToken: "t")))
        XCTAssertThrowsError(try HeraldoClient(credentials: Credentials(serverURL: "https://example.com")))
        let c = try? HeraldoClient(credentials: Credentials(serverURL: " https://example.com/heraldo ", readToken: "t", writeToken: ""))
        XCTAssertNotNil(c)
        XCTAssertNil(c?.writeToken, "an empty write token is no write token")
    }
}
