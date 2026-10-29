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

    func testDecodesPushTrace() throws {
        let json = """
        {"event_id": "01JBA6Y8Q2K9M3T4V5W6X7Y8Z9", "why": "muted by mute_1 (project)",
         "decisions": [
           {"stage": "hold", "outcome": "pass", "reason": "not held", "at": "2026-10-26T12:00:02Z"},
           {"stage": "mute", "outcome": "drop", "reason": "muted by mute_1 (project)",
            "detail": {"mute": "mute_1"}, "at": "2026-10-26T12:00:02.5Z"}
         ]}
        """
        let t = try JSON.decoder.decode(PushTrace.self, from: Data(json.utf8))
        XCTAssertEqual(t.why, "muted by mute_1 (project)")
        XCTAssertEqual(t.decisions.map(\.stage), ["hold", "mute"])
        XCTAssertEqual(t.decisions.last?.outcome, "drop")
        let event = """
        {"id": "x", "source": "s", "why": "passive: inbox only"}
        """
        let e = try JSON.decoder.decode(Event.self, from: Data(event.utf8))
        XCTAssertEqual(e.why, "passive: inbox only")
    }

    func testDecodesAttachments() throws {
        let json = """
        {
          "id": "01JBA6Z0000000000000000012",
          "source": "share:phone",
          "kind": "message",
          "attachments": ["att_01JBA6Z0000000000000000010", "att_01JBA6Z0000000000000000011"],
          "attachment_info": [
            {"id": "att_01JBA6Z0000000000000000010", "content_type": "image/png", "bytes": 48213},
            {"id": "att_01JBA6Z0000000000000000011", "content_type": "text/plain", "bytes": 912}
          ]
        }
        """
        let e = try JSON.decoder.decode(Event.self, from: Data(json.utf8))
        XCTAssertEqual(e.attachments?.count, 2)
        let info = try XCTUnwrap(e.attachmentInfo)
        XCTAssertEqual(info.map(\.contentType), ["image/png", "text/plain"])
        XCTAssertTrue(info[0].isImage)
        XCTAssertTrue(info[1].isText)
        XCTAssertEqual(info[0].bytes, 48213)
    }

    func testDecodesChecks() throws {
        let json = """
        {"checkins": [
          {"key": "backup", "host": "box-1", "state": "down", "down_reason": "missed",
           "state_since": "2026-10-27T13:10:00Z", "created_at": "2026-10-26T12:00:00Z",
           "schedule": {"period_s": 3600, "grace_s": 600, "reminder_s": 86400},
           "last_ping": "2026-10-27T12:00:00.5Z", "last_duration_ms": 102400,
           "next_due": "2026-10-27T13:00:00Z", "down_after": "2026-10-27T13:10:00Z",
           "last_log": "copied 3 files\\nall good\\n", "last_log_at": "2026-10-27T12:00:00Z",
           "incident": "open"},
          {"key": "weekly", "state": "sleeping", "state_since": "2026-10-27T13:10:00Z",
           "created_at": "2026-10-27T13:10:00Z",
           "schedule": {"cron": "0 4 * * 1", "tz": "Europe/Madrid", "grace_s": 3600, "max_runtime_s": 1800}}
        ]}
        """
        let list = try JSON.decoder.decode(CheckList.self, from: Data(json.utf8))
        let backup = list.checkins[0]
        XCTAssertEqual(backup.stateLabel, "Down")
        XCTAssertEqual(backup.lastDuration, "1m42s")
        XCTAssertEqual(backup.lastLogLine, "copied 3 files")
        XCTAssertEqual(backup.schedule?.summary, "every 1h, grace 10m")
        XCTAssertEqual(backup.nextDue, JSON.parseDate("2026-10-27T13:00:00Z"))
        let weekly = list.checkins[1]
        XCTAssertEqual(weekly.state, .new, "an unknown state reads as new")
        XCTAssertEqual(weekly.schedule?.summary, "cron 0 4 * * 1 Europe/Madrid, grace 1h, max runtime 30m")
        XCTAssertNil(weekly.lastDuration)
    }

    func testDecodesCheckinSchedule() throws {
        let json = #"{"id": "01A", "source": "cron", "kind": "checkin", "key": "k", "checkin": {"rid": "r", "schedule": {"period_s": 60}}}"#
        let e = try JSON.decoder.decode(Event.self, from: Data(json.utf8))
        XCTAssertEqual(e.checkin?.schedule?.periodS, 60)
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
        XCTAssertNil(p["critical_alerts"], "a build without the entitlement never sends it")
    }

    func testRegistrationEncodesCriticalAlerts() throws {
        let r = DeviceRegistration(token: "ab12", environment: .production, topic: "com.example.app",
                                   name: "Phone", platform: "iOS", appVersion: "1.0 (1)",
                                   permissions: Permissions(alerts: true, criticalAlerts: true))
        let obj = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(r)) as? [String: Any])
        let p = try XCTUnwrap(obj["permissions"] as? [String: Any])
        XCTAssertEqual(p["critical_alerts"] as? Bool, true)
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

    func testRoutine() throws {
        func event(_ json: String) throws -> Event {
            try JSON.decoder.decode(Event.self, from: Data(#"{"id":"01","source":"s",\#(json)}"#.utf8))
        }
        XCTAssertTrue(try event(#""kind":"checkin","key":"k","action":"success""#).isRoutine)
        XCTAssertTrue(try event(#""kind":"checkin","key":"k","action":"log","checkin":{"exit_code":1}"#).isRoutine)
        XCTAssertFalse(try event(#""kind":"checkin","key":"k","action":"fail""#).isRoutine)
        XCTAssertFalse(try event(#""kind":"checkin","key":"k","action":"success","checkin":{"exit_code":2}"#).isRoutine)
        XCTAssertTrue(try event(#""kind":"progress","key":"k","action":"update""#).isRoutine)
        XCTAssertTrue(try event(#""kind":"status","key":"k""#).isRoutine)
        XCTAssertFalse(try event(#""kind":"message""#).isRoutine)
        XCTAssertFalse(try event(#""kind":"incident","key":"k","action":"open""#).isRoutine)
        XCTAssertEqual(try event(#""kind":"message","project":"web""#).project, "web")
    }

    func testEventQueryItems() {
        let q = EventQuery(order: .desc, project: ["web", noProject], routine: .exclude)
        let items = q.items.map { "\($0.name)=\($0.value ?? "")" }
        XCTAssertEqual(items, ["order=desc", "routine=exclude", "project=web", "project=-"])
    }

    func testProjectList() throws {
        let json = #"{"projects":[{"name":"web","events":3,"last_at":"2026-10-28T12:00:00Z"}],"unfiled":2}"#
        let l = try JSON.decoder.decode(ProjectList.self, from: Data(json.utf8))
        XCTAssertEqual(l.projects.map(\.name), ["web"])
        XCTAssertEqual(l.unfiled, 2)
        let check = try JSON.decoder.decode(Check.self, from: Data(#"{"key":"job","state":"up","project":"web"}"#.utf8))
        XCTAssertEqual(check.project, "web")
        let mute = try JSON.encoder.encode(MuteInput(project: "web", for: "1h"))
        XCTAssertEqual(String(decoding: mute, as: UTF8.self), #"{"for":"1h","project":"web"}"#)
    }

    func testIncidentsText() throws {
        let json = #"""
        {"incidents":[
          {"id":"1","source":"uptime","host":"web-1","kind":"incident","action":"ack","title":"Web is down",
           "ts":"2026-10-27T12:00:00Z","incident":{"state":"acked","opened_at":"2026-10-27T11:48:00Z"}},
          {"id":"2","source":"backup","kind":"incident","action":"open","title":"Backup failed",
           "ts":"2026-10-27T10:00:00Z","incident":{"state":"open","opened_at":"2026-10-27T10:00:00Z"}}]}
        """#
        let state = try JSON.decoder.decode(ServerState.self, from: Data(json.utf8))
        let sorted = state.sortedIncidents
        XCTAssertEqual(sorted.map(\.id), ["2", "1"], "open before acked")
        XCTAssertEqual(ServerState.incidentsText(sorted), """
        open: Backup failed (backup), since 2026-10-27T10:00:00Z
        acked: Web is down (uptime, web-1), since 2026-10-27T11:48:00Z
        """)
        XCTAssertEqual(ServerState.incidentsText([]), "No open incidents")
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

    func testIncidentHoldAndHeldBack() throws {
        let json = """
        {"id": "e1", "source": "uptime", "kind": "incident", "key": "web", "action": "update",
         "incident": {"state": "acked", "opened_at": "2026-10-26T12:00:00Z"},
         "hold": {"state": "held", "reason": "flapping"},
         "delivery": [{"device": "dev_01", "outcome": "skipped", "at": "2026-10-26T12:00:02Z", "error": "muted by mute_1"}]}
        """
        let e = try JSON.decoder.decode(Event.self, from: Data(json.utf8))
        XCTAssertTrue(e.isAcked, "an update after an ack is still acked")
        XCTAssertEqual(e.incident?.openedAt, JSON.parseDate("2026-10-26T12:00:00Z"))
        XCTAssertNil(e.incident?.openFor)
        XCTAssertEqual(e.hold?.label, "held · flapping")
        XCTAssertTrue(e.wasHeldBack)
        XCTAssertNil(e.incident?.stale, "an older server sends no stale flag")

        let stale = #"{"id": "e3", "source": "uptime", "kind": "incident", "key": "web", "incident": {"state": "open", "opened_at": "2026-10-20T12:00:00Z", "stale": true}}"#
        XCTAssertEqual(try JSON.decoder.decode(Event.self, from: Data(stale.utf8)).incident?.stale, true)

        let resolved = IncidentInfo(state: "resolved", openedAt: Date(timeIntervalSince1970: 0),
                                    resolvedAt: Date(timeIntervalSince1970: 720))
        XCTAssertEqual(resolved.openFor, 720)
        // An older server sends no incident: the action decides.
        var old = Event(id: "e2", source: "s", kind: "incident")
        old.action = "ack"
        XCTAssertTrue(old.isAcked)
    }

    func testMuteDecodesAndActionTargets() throws {
        let json = #"{"id": "mute_1", "source": "backup", "until": "2026-10-27T13:00:00Z", "created_at": "2026-10-27T12:00:00Z", "active": true}"#
        let m = try JSON.decoder.decode(Mute.self, from: Data(json.utf8))
        XCTAssertEqual(m.source, "backup")
        XCTAssertTrue(m.active)
        let body = try JSONEncoder().encode(MuteInput(source: "backup", for: "1h"))
        XCTAssertEqual(try JSONSerialization.jsonObject(with: body) as? [String: String], ["source": "backup", "for": "1h"])

        let incident = PushInfo(userInfo: ["heraldo": ["id": "x", "kind": "incident", "source": "uptime", "key": "web",
                                                       "icon": "uptime", "icon_url": "https://example.com/i.png"]])
        XCTAssertEqual(incident?.icon, "uptime")
        XCTAssertEqual(incident?.iconURL, "https://example.com/i.png")
        XCTAssertEqual(incident.flatMap(PushCategory.muteInput), MuteInput(key: "web", for: "1h"))
        let event = PushInfo(userInfo: ["heraldo": ["id": "y", "kind": "message", "source": "backup", "key": "k"]])
        XCTAssertEqual(event.flatMap(PushCategory.muteInput), MuteInput(source: "backup", for: "1h"))
        let run = PushInfo(userInfo: ["heraldo": ["id": "z", "kind": "checkin", "source": "checkin", "key": "nightly"]])
        XCTAssertEqual(run.flatMap(PushCategory.muteInput), MuteInput(key: "checkin:nightly", for: "1h"))
        // Snooze 1 h: incidents only, with remind.
        XCTAssertEqual(incident.flatMap(PushCategory.snoozeInput), MuteInput(key: "web", for: "1h", remind: true))
        XCTAssertNil(event.flatMap(PushCategory.snoozeInput))
        XCTAssertNil(run.flatMap(PushCategory.snoozeInput))
    }

    func testSnoozeAndFlapCoding() throws {
        // A plain mute leaves remind out, so a server before 0.7 takes it.
        let plain = String(decoding: try JSON.encoder.encode(MuteInput(key: "k", for: "1h")), as: UTF8.self)
        XCTAssertFalse(plain.contains("remind"))
        let snooze = String(decoding: try JSON.encoder.encode(MuteInput(key: "k", for: "1h", remind: true)), as: UTF8.self)
        XCTAssertTrue(snooze.contains(#""remind":true"#))
        let m = try JSON.decoder.decode(Mute.self, from: Data(#"""
            {"id":"mute_1","key":"k","until":"2026-10-26T13:00:00Z","active":false,"remind":true,
             "reminded_at":"2026-10-26T13:00:05Z"}
            """#.utf8))
        XCTAssertTrue(m.isSnooze)
        XCTAssertNotNil(m.remindedAt)
        let e = try JSON.decoder.decode(Event.self, from: Data(#"""
            {"id":"a","source":"uptime","kind":"incident","key":"host:web-1","action":"open",
             "incident":{"state":"open","opened_at":"2026-10-26T12:40:01Z","flapping":true,"flap_count":4,
             "flap_since":"2026-10-26T12:00:01Z"}}
            """#.utf8))
        XCTAssertEqual(e.incident?.isFlapping, true)
        XCTAssertEqual(e.incident?.flapCount, 4)
        let old = try JSON.decoder.decode(Event.self, from: Data(#"""
            {"id":"b","source":"uptime","kind":"incident","incident":{"state":"open","opened_at":"2026-10-26T12:40:01Z"}}
            """#.utf8))
        XCTAssertEqual(old.incident?.isFlapping, false)
    }

    func testCacheHoldStatesAndResolved() {
        var c = HistoryCache()
        var a = Event(id: "a", source: "s", kind: "incident")
        a.key = "web"; a.action = "resolve"; a.ts = Date(timeIntervalSince1970: 2000)
        var b = a
        b.id = "b"; b.ts = Date(timeIntervalSince1970: 1000)
        var h = Event(id: "h", source: "tee")
        h.hold = Hold(state: "released")
        c.prepend([a, b, h])
        XCTAssertEqual(c.holdStates, ["held", "released"])
        XCTAssertEqual(c.resolvedIncidents(since: Date(timeIntervalSince1970: 0)).map(\.id), ["a"])
        XCTAssertTrue(c.resolvedIncidents(since: Date(timeIntervalSince1970: 3000)).isEmpty)
    }
}
