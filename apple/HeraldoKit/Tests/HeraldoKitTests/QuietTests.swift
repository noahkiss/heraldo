import XCTest
@testable import HeraldoKit

/// The badge, server settings and the Focus filter, from hand-written
/// fixtures.
final class QuietTests: XCTestCase {
    func testBadgeFallsBackToUnread() throws {
        let new = try JSON.decoder.decode(ReadCursor.self, from: Data(#"{"name":"inbox","cursor":"s9","unread":7,"badge":2}"#.utf8))
        let state = UnreadState(cursor: new, ids: [])
        XCTAssertEqual(state.inboxCount, 7)
        XCTAssertEqual(state.badge, 2)
        // A server before 0.7 has no badge: unread is the badge.
        let old = try JSON.decoder.decode(ReadCursor.self, from: Data(#"{"name":"inbox","cursor":"s9","unread":7}"#.utf8))
        XCTAssertEqual(UnreadState(cursor: old, ids: []).badge, 7)
    }

    func testDecodesSettings() throws {
        let json = """
        {"settings": [
          {"key": "quiet.enabled", "type": "bool", "value": true, "default": false, "source": "db",
           "secret": false, "description": "Quiet hours on.", "env": "HERALDO_QUIET_ENABLED"},
          {"key": "badge.scope", "type": "enum", "value": "attention", "source": "env", "locked_by": "HERALDO_BADGE_SCOPE",
           "enum": ["attention", "pushed", "none"], "secret": false, "description": "What the badge counts.", "env": "HERALDO_BADGE_SCOPE"},
          {"key": "badge.projects", "type": "list", "value": ["home"], "source": "db", "secret": false,
           "description": "Projects.", "env": "HERALDO_BADGE_PROJECTS"},
          {"key": "llm.token", "type": "secret", "set": false, "source": "default", "secret": true,
           "description": "Token.", "env": "HERALDO_LLM_TOKEN"}
        ]}
        """
        let list = try JSON.decoder.decode(ServerSettingList.self, from: Data(json.utf8))
        XCTAssertEqual(list.settings.count, 4)
        XCTAssertEqual(list.settings[0].value, .bool(true))
        XCTAssertTrue(list.settings[1].isLocked)
        XCTAssertEqual(list.settings[1].enum ?? [], ["attention", "pushed", "none"])
        XCTAssertEqual(list.settings[2].value?.list, ["home"])
        XCTAssertNil(list.settings[3].value)
        XCTAssertEqual(list.settings[3].set, false, "a secret reports only whether it is set")
        let body = try JSONEncoder().encode(SettingInput(value: .string("22:00")))
        XCTAssertEqual(String(decoding: body, as: UTF8.self), #"{"value":"22:00"}"#)
    }

    func testFocusFilterFromParameters() throws {
        XCTAssertEqual(FocusFilterState.from(projects: [], breakThrough: "time-sensitive"), FocusFilterState(active: false))
        let sleep = FocusFilterState.from(projects: [], breakThrough: "critical")
        XCTAssertEqual(sleep, FocusFilterState(active: true, breakThrough: "critical"))
        let work = FocusFilterState.from(projects: ["infra"], breakThrough: "time-sensitive")
        XCTAssertEqual(work.projects, ["infra"])
        XCTAssertTrue(work.active)
    }

    func testRegistrationLeavesFocusOut() throws {
        var reg = DeviceRegistration(token: "ab", environment: .production, topic: "t", name: nil,
                                     platform: nil, appVersion: nil, permissions: nil)
        XCTAssertFalse(String(decoding: try JSONEncoder().encode(reg), as: UTF8.self).contains("focus_filter"))
        reg.focusFilter = FocusFilterState(active: true, breakThrough: "critical")
        let s = String(decoding: try JSONEncoder().encode(reg), as: UTF8.self)
        XCTAssertTrue(s.contains(#""focus_filter":{"#) && s.contains(#""break_through":"critical""#), s)
    }
}
