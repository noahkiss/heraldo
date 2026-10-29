import XCTest
@testable import HeraldoKit

/// A stand-in for iCloud's key-value store.
final class FakeCloud: PrefsCloud {
    var values: [String: Any] = [:]
    var available = true
    func object(forKey key: String) -> Any? { values[key] }
    func set(_ value: Any?, forKey key: String) { values[key] = value }
    func removeObject(forKey key: String) { values[key] = nil }
    func synchronize() -> Bool { available }
}

final class SyncedPrefsTests: XCTestCase {
    private func defaults() -> UserDefaults {
        let name = "prefs-test-\(UUID().uuidString)"
        let d = UserDefaults(suiteName: name)!
        d.removePersistentDomain(forName: name)
        return d
    }

    func testWritesGoToBothAndDefaultsHold() {
        let cloud = FakeCloud()
        let p = SyncedPrefs(local: defaults(), cloud: cloud)
        XCTAssertTrue(p.syncs)
        XCTAssertEqual(p.groupBy, .thread)
        p.groupBy = .project
        p.shareProject = "homelab"
        p.inboxFilter = InboxFilter(kind: "incident", showRoutine: true)
        XCTAssertEqual(cloud.values["notify.groupBy"] as? String, "project")
        XCTAssertEqual(cloud.values["share.project"] as? String, "homelab")
        XCTAssertEqual(cloud.values["inbox.showRoutine"] as? Bool, true)
        XCTAssertEqual(p.inboxFilter, InboxFilter(kind: "incident", showRoutine: true))
        p.groupBy = .thread
        p.shareProject = ""
        XCTAssertNil(cloud.values["notify.groupBy"], "the default is stored as no value")
        XCTAssertNil(cloud.values["share.project"])
        XCTAssertNil(p.shareProject)
    }

    func testNoEntitlementStaysLocal() {
        let cloud = FakeCloud()
        cloud.available = false
        let local = defaults()
        let p = SyncedPrefs(local: local, cloud: cloud)
        XCTAssertFalse(p.syncs)
        p.groupBy = .project
        XCTAssertTrue(cloud.values.isEmpty, "a build without the entitlement never writes to iCloud")
        XCTAssertEqual(SyncedPrefs(local: local, cloud: nil).groupBy, .project, "the mirror keeps it")
    }

    func testCloudWinsAndFirstRunSeeds() {
        let local = defaults()
        SyncedPrefs(local: local, cloud: nil).shareProject = "mine"
        SyncedPrefs(local: local, cloud: nil).groupBy = .project
        let cloud = FakeCloud()
        cloud.values["share.project"] = "theirs"
        let p = SyncedPrefs(local: local, cloud: cloud)
        XCTAssertEqual(p.shareProject, "theirs", "iCloud's value replaces the mirror's")
        XCTAssertEqual(cloud.values["notify.groupBy"] as? String, "project", "a key iCloud lacks is seeded")

        // Later runs do not seed again: a key removed elsewhere stays removed.
        cloud.values["notify.groupBy"] = nil
        let again = SyncedPrefs(local: local, cloud: cloud)
        XCTAssertNil(cloud.values["notify.groupBy"])
        _ = again
    }

    func testPullCopiesChangedKeys() {
        let cloud = FakeCloud()
        let p = SyncedPrefs(local: defaults(), cloud: cloud)
        p.shareProject = "a"
        cloud.values["share.project"] = nil
        cloud.values["notify.groupBy"] = "project"
        let keys = p.pull([NSUbiquitousKeyValueStoreChangedKeysKey: ["notify.groupBy", "share.project", "other"]])
        XCTAssertEqual(Set(keys), [.groupBy, .shareProject])
        XCTAssertEqual(p.groupBy, .project)
        XCTAssertNil(p.shareProject, "a key removed in iCloud goes from the mirror")
    }

    func testShareProjectIsTheShareSheetsValue() {
        let local = defaults()
        let cloud = FakeCloud()
        let p = SyncedPrefs(local: local, cloud: cloud)
        // A share extension writes its own key in the app group.
        local.set("infra", forKey: SharePrefs.lastProjectKey)
        XCTAssertEqual(p.shareProject, "infra", "the mirror is the share sheet's key")
        XCTAssertNil(cloud.values["share.project"])
        XCTAssertTrue(p.pushMirror())
        XCTAssertEqual(cloud.values["share.project"] as? String, "infra")
        XCTAssertFalse(p.pushMirror(), "nothing new to copy")
        p.shareProject = "home"
        XCTAssertEqual(local.string(forKey: SharePrefs.lastProjectKey), "home")
    }

    func testGroupByThread() {
        XCTAssertNil(GroupBy.thread.threadIdentifier(project: "a"))
        XCTAssertEqual(GroupBy.project.threadIdentifier(project: "a"), "project:a")
        XCTAssertNil(GroupBy.project.threadIdentifier(project: " "), "no project keeps the server's thread")
        XCTAssertNil(GroupBy.project.threadIdentifier(project: nil))
    }

    func testPushInfoProject() {
        let info = PushInfo(userInfo: ["heraldo": ["id": "x", "project": "homelab"]])
        XCTAssertEqual(info?.project, "homelab")
        XCTAssertNil(PushInfo(userInfo: ["heraldo": ["id": "x", "project": ""]])?.project)
    }
}

final class WidgetSnapshotTests: XCTestCase {
    private func event(_ id: String, kind: String, action: String? = nil, project: String? = nil,
                       ackState: String? = nil) throws -> Event {
        var json: [String: Any] = ["id": id, "source": "s", "kind": kind, "title": id]
        if let action { json["action"] = action }
        if let project { json["project"] = project }
        if let ackState { json["incident"] = ["state": ackState] }
        return try JSON.decoder.decode(Event.self, from: JSONSerialization.data(withJSONObject: json))
    }

    func testCountsAndProjects() throws {
        let incidents = [try event("open", kind: "incident", action: "open", project: "a"),
                         try event("acked", kind: "incident", action: "open", project: "a", ackState: "acked"),
                         try event("other", kind: "incident", action: "update", project: "b")]
        let events = [try event("m1", kind: "message", project: "a"),
                      try event("ping", kind: "checkin", project: "a"),
                      try event("m2", kind: "message", project: "a"),
                      try event("m3", kind: "message", project: "a"),
                      try event("m4", kind: "message", project: "a"),
                      try event("loose", kind: "message")]
        let s = WidgetSnapshot(incidents: incidents, checks: [], projects: ["a", "b"], events: events)
        XCTAssertEqual(Set(s.incidents.map(\.id)), ["open", "other"], "acknowledged incidents do not count")
        XCTAssertEqual(s.recent.map(\.id), ["m1", "m2", "m3"], "routine events stay out; three per project")
        let a = s.project("a")
        XCTAssertEqual(a.incidents.map(\.id), ["open"])
        XCTAssertEqual(a.recent.count, 3)
        XCTAssertTrue(s.project("c").incidents.isEmpty)
    }
}

/// Serves canned `GET /v1/events` pages, with or without the routine header.
final class RoutineStubProtocol: URLProtocol {
    nonisolated(unsafe) static var handler: ((URLRequest) -> (Int, [String: String], Data))?

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        guard let (status, headers, data) = Self.handler?(request) else { return }
        let r = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: nil, headerFields: headers)!
        client?.urlProtocol(self, didReceive: r, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: data)
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

final class RoutineSyncTests: XCTestCase {
    private var session: URLSession!
    private var queries: [[String: String]] = []

    override func setUp() {
        let c = URLSessionConfiguration.ephemeral
        c.protocolClasses = [RoutineStubProtocol.self]
        session = URLSession(configuration: c)
        queries = []
    }

    override func tearDown() { RoutineStubProtocol.handler = nil }

    private func row(_ i: Int, routine: Bool) -> [String: Any] {
        ["id": "e\(i)", "source": "s", "kind": routine ? "checkin" : "message", "title": "e\(i)"]
    }

    /// An old server: it ignores `routine`, sends no header and pages every row.
    func testOldServerKeepsPagingThroughRoutineRows() async throws {
        // Ten rows, newest first; rows 9 to 6 are routine pings.
        let rows = (0..<10).reversed().map { row($0, routine: $0 >= 6) }
        RoutineStubProtocol.handler = { [unowned self] req in
            let items = URLComponents(url: req.url!, resolvingAgainstBaseURL: false)?.queryItems ?? []
            let q = Dictionary(items.map { ($0.name, $0.value ?? "") }, uniquingKeysWith: { a, _ in a })
            self.queries.append(q)
            let limit = Int(q["limit"] ?? "") ?? 50
            let start = q["before"].flatMap(Int.init) ?? 0
            let page = Array(rows.dropFirst(start).prefix(limit))
            let next = start + page.count
            let body: [String: Any] = ["events": page, "next_cursor": String(next)]
            return (200, ["Content-Type": "application/json"], try! JSONSerialization.data(withJSONObject: body))
        }
        let client = HeraldoClient(baseURL: URL(string: "http://stub.invalid")!, readToken: "t", session: session)
        let sync = HistorySync(client: client, pageSize: 4)
        var cache = HistoryCache()
        // The newest page is all pings.
        let first = try await client.listEvents(EventQuery(order: .desc, limit: 4, routine: .exclude))
        XCTAssertEqual(first.events.count, 0, "the client drops the routine rows itself")
        XCTAssertEqual(first.fetchedCount, 4, "but counts the rows the server sent")

        cache.olderCursor = "0"
        cache = try await sync.loadOlder(cache)
        XCTAssertFalse(cache.reachedEnd, "a page of pings is full, not the end")
        cache = try await sync.loadOlder(cache)
        XCTAssertEqual(cache.events.map(\.id), ["e5", "e4", "e3", "e2"])
        XCTAssertFalse(cache.reachedEnd)
        cache = try await sync.loadOlder(cache)
        XCTAssertEqual(cache.events.map(\.id), ["e5", "e4", "e3", "e2", "e1", "e0"])
        XCTAssertTrue(cache.reachedEnd)
        XCTAssertTrue(queries.allSatisfy { $0["routine"] == "exclude" }, "the cache asks for routine=exclude")
    }

    /// A 0.6.0+ server filters and says so; the page is taken as sent.
    func testNewServerFilters() async throws {
        RoutineStubProtocol.handler = { _ in
            let body: [String: Any] = ["events": [self.row(1, routine: false)], "next_cursor": "1"]
            return (200, ["Content-Type": "application/json", "Heraldo-Routine": "exclude"],
                    try! JSONSerialization.data(withJSONObject: body))
        }
        let client = HeraldoClient(baseURL: URL(string: "http://stub.invalid")!, readToken: "t", session: session)
        let page = try await client.listEvents(EventQuery(order: .desc, limit: 4, routine: .exclude))
        XCTAssertNil(page.fetched)
        XCTAssertEqual(page.fetchedCount, 1)
    }
}
