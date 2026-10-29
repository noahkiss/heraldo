import XCTest
@testable import HeraldoKit

/// Against a real heraldo-server. `apple/scripts/test-server.sh` starts one
/// with a temp database and sets the variables; without them these skip.
/// The server is shared by every test, so each test uses its own source and
/// cursor name.
final class ServerTests: XCTestCase {
    var client: HeraldoClient!

    override func setUpWithError() throws {
        let env = ProcessInfo.processInfo.environment
        guard let url = env["HERALDO_TEST_URL"].flatMap(URL.init(string:)),
              let read = env["HERALDO_TEST_READ_TOKEN"], let write = env["HERALDO_TEST_WRITE_TOKEN"] else {
            throw XCTSkip("no test server; run apple/scripts/test-server.sh")
        }
        client = HeraldoClient(baseURL: url, readToken: read, writeToken: write)
    }

    private func unique(_ prefix: String) -> String {
        "\(prefix)-\(UUID().uuidString.prefix(8).lowercased())"
    }

    @discardableResult
    private func post(_ n: Int, source: String) async throws -> [Event] {
        var out: [Event] = []
        for i in 1...n {
            out.append(try await client.post(EventInput(source: source, kind: "message", title: "Test \(i)",
                                                         body: "Body **\(i)**")))
        }
        return out
    }

    func testHealth() async throws {
        let h = try await client.health()
        XCTAssertEqual(h.status, "ok")
    }

    func testTokenScopes() async throws {
        var readOnly = client!
        readOnly.writeToken = readOnly.readToken
        do {
            try await readOnly.post(EventInput(source: "x", title: "no"))
            XCTFail("a read token posted")
        } catch let e as HeraldoError {
            XCTAssertEqual(e.status, 403)
        }
        var noWrite = client!
        noWrite.writeToken = nil
        do {
            try await noWrite.post(EventInput(source: "x"))
            XCTFail("posted without a write token")
        } catch let e as HeraldoError {
            XCTAssertEqual(e, .notConfigured("The write token"))
        }
        var bad = client!
        bad.readToken = "nope"
        do {
            _ = try await bad.state()
            XCTFail("an unknown token read")
        } catch let e as HeraldoError {
            XCTAssertEqual(e.status, 401)
        }
    }

    func testCheckWriteTokenStoresNothing() async throws {
        let before = try await client.health().events
        try await client.checkWriteToken()
        let after = try await client.health().events
        XCTAssertEqual(after, before, "the check stored an event")
        var readOnly = client!
        readOnly.writeToken = readOnly.readToken
        do {
            try await readOnly.checkWriteToken()
            XCTFail("a read token passed the write check")
        } catch let e as HeraldoError {
            XCTAssertEqual(e.status, 403)
        }
        var unknown = client!
        unknown.writeToken = "nope"
        do {
            try await unknown.checkWriteToken()
            XCTFail("an unknown token passed the write check")
        } catch let e as HeraldoError {
            XCTAssertEqual(e.status, 401)
        }
    }

    func testSyncPagesAndOlder() async throws {
        let sync = HistorySync(client: client, pageSize: 3)
        try await post(4, source: unique("sync"))
        var cache = try await sync.sync(HistoryCache())
        XCTAssertEqual(cache.events.count, 3, "a fresh cache takes one page, newest first")
        XCTAssertNotNil(cache.syncCursor)
        let newest = cache.events[0].id

        let source = unique("sync")
        let more = try await post(7, source: source)
        cache = try await sync.sync(cache)
        XCTAssertEqual(Array(cache.events.prefix(7).map(\.id)), more.reversed().map(\.id),
                       "every event after the cursor, over several pages, newest first")
        XCTAssertEqual(cache.events[7].id, newest)
        XCTAssertEqual(Set(cache.events.map(\.id)).count, cache.events.count, "no copies")

        let again = try await sync.sync(cache)
        XCTAssertEqual(again, cache, "an up-to-date cache stays the same")

        let older = try await sync.loadOlder(cache)
        XCTAssertGreaterThan(older.events.count, cache.events.count)
        XCTAssertEqual(Array(older.events.prefix(cache.events.count)), cache.events)

        let found = try await sync.search(q: nil, kind: "message", source: source)
        XCTAssertEqual(found.events.map(\.id), Array(more.reversed().map(\.id).prefix(3)))
        let full = try await client.event(id: more[0].id)
        XCTAssertEqual(full.body, "Body **1**")
    }

    func testReadCursorMovesForwardOnly() async throws {
        let sync = HistorySync(client: client, pageSize: 4)
        let name = unique("test")
        _ = try await post(1, source: unique("cursor"))
        let first = try await sync.unread(name)
        XCTAssertNotNil(first.cursor, "a missing cursor starts at the newest event")
        XCTAssertEqual(first.ids, [], "the history before it is read")
        let made = try await client.cursor(name)
        XCTAssertEqual(made?.cursor, first.cursor?.cursor, "the server keeps it")

        let evs = try await post(6, source: unique("cursor"))
        let moved = try await sync.markRead(eventID: evs[2].id, name)
        XCTAssertNotNil(moved)
        var state = try await sync.unread(name)
        XCTAssertEqual(state.ids, Set(evs[3...].map(\.id)), "events after the third are unread")
        XCTAssertEqual(state.badge, 3)

        // An earlier event: the cursor stays where it is.
        let back = try await sync.markRead(eventID: evs[0].id, name)
        XCTAssertEqual(back?.cursor, moved?.cursor)
        state = try await sync.unread(name)
        XCTAssertEqual(state.ids.count, 3)

        try await sync.markAllRead(name)
        state = try await sync.unread(name)
        XCTAssertEqual(state.ids, [])
        XCTAssertEqual(state.badge, 0)

        let unknown = try await sync.markRead(eventID: "01NOTANEVENT0000000000000", name)
        XCTAssertNil(unknown)
    }

    func testMarkReadByEventCursor() async throws {
        let sync = HistorySync(client: client)
        let name = unique("test")
        let evs = try await post(3, source: unique("own-cursor"))
        let listed = try await client.listEvents(EventQuery(order: .asc, limit: 10, source: evs[0].source)).events
        XCTAssertEqual(listed.map(\.id), evs.map(\.id))
        XCTAssertNotNil(listed[1].cursor, "a listed event carries its cursor")
        let moved = try await sync.markRead(listed[1], name)
        XCTAssertEqual(moved?.cursor, listed[1].cursor!)
        let state = try await sync.unread(name)
        XCTAssertTrue(state.ids.isSuperset(of: [evs[2].id]))
        XCTAssertFalse(state.ids.contains(evs[1].id))
    }

    func testCursorOfFindsDeepEvents() async throws {
        let sync = HistorySync(client: client, pageSize: 2)
        let evs = try await post(5, source: unique("deep"))
        let c = try await sync.cursorOf(eventID: evs[0].id)
        let page = try await client.listEvents(EventQuery(after: c, order: .asc, limit: 10))
        XCTAssertEqual(page.events.first?.id, evs[1].id, "the cursor sits right after the event")
    }

    func testDeviceRegistrationAndPermissionHealth() async throws {
        let token = (0..<32).map { _ in String(format: "%02x", UInt8.random(in: 0...255)) }.joined()
        var reg = DeviceRegistration(token: token, environment: .sandbox, topic: "com.example.heraldo",
                                     name: "Test phone", platform: "iOS", appVersion: "0.1.0 (1)",
                                     permissions: Permissions(alerts: true, timeSensitive: false, liveActivities: true))
        let dev = try await client.register(reg)
        XCTAssertTrue(dev.active)
        XCTAssertEqual(dev.tokenSuffix, String(token.suffix(8)))
        XCTAssertEqual(dev.permissions?.timeSensitive, false)

        let key = "device:\(dev.id):time_sensitive"
        var st = try await client.state()
        let incident = try XCTUnwrap(st.incidents.first { $0.key == key }, "a permission off opens an incident")
        XCTAssertEqual(incident.source, "heraldo")

        // Ack it with the write token, as the notification action does.
        try await client.ack(incident, source: AppInfo.source)
        st = try await client.state()
        XCTAssertEqual(st.incidents.first { $0.key == key }?.action, "ack")

        // Re-registering the same token updates the device; permission back on resolves.
        reg.permissions?.timeSensitive = true
        let again = try await client.register(reg)
        XCTAssertEqual(again.id, dev.id)
        st = try await client.state()
        XCTAssertNil(st.incidents.first { $0.key == key }, "the incident resolved")

        reg.token = String(token.reversed())
        let moved = try await client.updateDevice(id: dev.id, reg)
        XCTAssertEqual(moved.tokenSuffix, String(reg.token.suffix(8)))
    }

    /// A progress start, update and end from LivePatch, read back with
    /// activity(key:). The test server runs with push off, so no instances.
    func testLiveActivityRoundTrip() async throws {
        let source = unique("live")
        let key = "\(source)/print"
        var f = LiveFields()
        f.title = "bracket.3mf"
        f.progress = 10
        f.status = "Printing"
        f.metricsText = "Nozzle: 215 °C"
        try await client.post(EventInput(source: source, kind: "progress", key: key, action: "start", content: f.patch()))
        var u = LiveFields()
        u.progress = 50
        try await client.post(EventInput(source: source, kind: "progress", key: key, action: "update",
                                         content: u.patch(clearing: [.metrics])))
        var a = try await client.activity(key: key)
        XCTAssertEqual(a.state, "active")
        XCTAssertEqual(a.source, source)
        XCTAssertTrue(a.instances.isEmpty)
        var end = LivePatch()
        end.status = .set("Done")
        end.keepFor = .set(60)
        try await client.post(EventInput(source: source, kind: "progress", key: key, action: "end", content: end))
        a = try await client.activity(key: key)
        XCTAssertEqual(a.state, "ended")
        XCTAssertEqual(a.endReason, "sender")
        do {
            _ = try await client.reportInstance(id: "lai_none", InstanceReport(state: "dismissed"))
            XCTFail("an unknown instance was accepted")
        } catch let e as HeraldoError {
            XCTAssertEqual(e.status, 404)
        }
    }

    func testMuteRoundTrip() async throws {
        let source = unique("mute")
        let m = try await client.mute(MuteInput(source: source, for: "1h"))
        XCTAssertTrue(m.active)
        XCTAssertEqual(m.source, source)
        let listed = try await client.mutes()
        XCTAssertTrue(listed.contains { $0.id == m.id })
        let ended = try await client.endMute(id: m.id)
        XCTAssertFalse(ended.active)
        XCTAssertNotNil(ended.endedAt)
        let after = try await client.mutes()
        XCTAssertFalse(after.contains { $0.id == m.id })
    }

    func testProjectMuteAndRoutineFilter() async throws {
        let project = unique("proj")
        let m = try await client.mute(MuteInput(project: project, for: "1h"))
        XCTAssertEqual(m.project, project)
        XCTAssertNil(m.source)
        _ = try await client.endMute(id: m.id)

        let source = unique("routine")
        try await client.post(EventInput(source: source, kind: "message", title: "hello"))
        try await client.post(EventInput(source: source, kind: "checkin", key: source, action: "success"))
        try await client.post(EventInput(source: source, kind: "checkin", key: source, action: "fail"))
        let main = try await client.listEvents(EventQuery(source: source, routine: .exclude)).events
        XCTAssertEqual(main.map(\.kind), ["message", "checkin"])
        let routine = try await client.listEvents(EventQuery(source: source, routine: .only)).events
        XCTAssertEqual(routine.map(\.action), ["success"])
        let history = try await client.listEvents(EventQuery(key: source)).events
        XCTAssertEqual(history.count, 2)
        let projects = try await client.projects()
        XCTAssertNotNil(projects)
    }

    func testIncidentStateAndHoldFilter() async throws {
        let key = unique("inc")
        try await client.post(EventInput(source: key, kind: "incident", key: key, action: "open", title: "Down"))
        let open = try await client.state().incidents.first { $0.key == key }
        XCTAssertEqual(open?.incident?.state, "open")
        XCTAssertNotNil(open?.incident?.openedAt)
        try await client.ack(XCTUnwrap(open), source: key)
        try await client.post(EventInput(source: key, kind: "incident", key: key, action: "update", title: "Still down"))
        let acked = try await client.state().incidents.first { $0.key == key }
        XCTAssertEqual(acked?.action, "update")
        XCTAssertTrue(acked?.isAcked ?? false, "an update after an ack stays acked")
        let resolved = try await client.post(EventInput(source: key, kind: "incident", key: key, action: "resolve"))
        XCTAssertEqual(resolved.incident?.state, "resolved")
        // The hold filter reaches the server; nothing from this test is held.
        let held = try await client.listEvents(EventQuery(source: key, hold: "held"))
        XCTAssertTrue(held.events.isEmpty)
    }
}
