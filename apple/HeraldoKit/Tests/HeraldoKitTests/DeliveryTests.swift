import XCTest
@testable import HeraldoKit

/// Delivery settings: the stamp on events, the rows, the preview, sources,
/// the `hidden` filter and the requests the client sends. The fixtures are
/// hand-written.
final class DeliveryTests: XCTestCase {
    override func tearDown() {
        DeliveryStub.reset()
        super.tearDown()
    }

    // MARK: Decoding

    func testDecodesStampOnEvent() throws {
        let json = """
        {"id": "01JBA7D0000000000000000000", "source": "uptime", "kind": "incident", "action": "open",
         "level": "time-sensitive",
         "deliver": {"as": "banner", "by": "dlv_01", "level_was": "time-sensitive", "badge": "off",
                     "wanted": "silent", "refused": "incident floor"}}
        """
        let e = try JSON.decoder.decode(Event.self, from: Data(json.utf8))
        let d = try XCTUnwrap(e.deliver)
        XCTAssertEqual(d.outcome, .banner)
        XCTAssertEqual(d.wantedOutcome, .silent)
        XCTAssertEqual(d.badgeChoice, .off)
        XCTAssertFalse(e.isHidden)
        XCTAssertEqual(d.summary(), "Delivered as a banner by a delivery setting · wanted silent, refused by the incident floor · badge off")
    }

    func testUnknownStampValuesStillDecode() throws {
        // A later server's outcome must not fail the page.
        let json = """
        {"events": [{"id": "e1", "source": "s", "deliver": {"as": "whisper", "badge": "sometimes"}}],
         "next_cursor": "c1"}
        """
        let page = try JSON.decoder.decode(EventPage.self, from: Data(json.utf8))
        XCTAssertNil(page.events[0].deliver?.outcome)
        XCTAssertFalse(page.events[0].isHidden)
        XCTAssertEqual(page.events[0].deliver?.summary(), "Delivery whisper · badge sometimes")
    }

    func testHiddenFollowsAppliedOutcomeOnly() {
        XCTAssertTrue(DeliverStamp(applied: "digest").isHidden)
        XCTAssertTrue(DeliverStamp(applied: "drop").isHidden)
        XCTAssertFalse(DeliverStamp(applied: "silent").isHidden)
        // The digest is off: the row asked for digest, the server applied silent.
        let s = DeliverStamp(applied: "silent", by: "dlv_1", levelWas: "active", badge: "auto", wanted: "digest")
        XCTAssertFalse(s.isHidden)
        XCTAssertEqual(s.summary(note: "Printer chatter"),
                       "Delivered silently by \"Printer chatter\" · was active · wanted digest; the digest is off")
    }

    func testSummaryOfFilledLevelOnNewSource() {
        let s = DeliverStamp(applied: "banner", badge: "auto", filled: "passive", newSource: true)
        XCTAssertEqual(s.summary(), "Delivered as a banner · level filled as passive · new source")
    }

    func testDecodesSettingList() throws {
        let json = """
        {"enabled": true, "new_source_level": "passive", "settings": [
          {"id": "dlv_a", "match": {"source": "printer", "level": ["passive", "active"]}, "deliver": "digest",
           "badge": "off", "position": 1, "note": "Printer chatter",
           "created_at": "2026-11-20T10:00:00Z", "updated_at": "2026-11-20T10:05:00.5Z", "updated_by": "admin"},
          {"id": "dlv_b", "match": {}, "badge": "auto", "default_level": "passive", "position": 2,
           "created_at": "2026-11-20T10:00:00Z", "updated_at": "2026-11-20T10:00:00Z"}
        ]}
        """
        let list = try JSON.decoder.decode(DeliverySettingList.self, from: Data(json.utf8))
        XCTAssertTrue(list.enabled)
        XCTAssertEqual(list.newSourceLevel, "passive")
        XCTAssertEqual(list.settings.count, 2)
        let a = list.settings[0]
        XCTAssertEqual(a.match, DeliveryMatch(source: "printer", level: ["passive", "active"]))
        XCTAssertEqual(a.deliver, .digest)
        XCTAssertEqual(a.badge, .off)
        XCTAssertEqual(a.effect, "Digest · badge off")
        XCTAssertEqual(a.match.summary, "source printer · level passive, active")
        XCTAssertEqual(a.updatedBy, "admin")
        let b = list.settings[1]
        XCTAssertNil(b.deliver)
        XCTAssertEqual(b.effect, "fills passive")
        XCTAssertEqual(b.match.summary, "Every event")
    }

    func testDecodesPreview() throws {
        let json = """
        {"days": 30, "events": 120,
         "outcomes": {"banner": 3, "silent": 10, "digest": 4, "drop": 1, "unchanged": 102},
         "rows": [{"index": 0, "hits": 15, "refused": 2}],
         "badge_joined": 0, "badge_left": 5,
         "changed": [{"id": "e1", "source": "printer", "title": "Toner low", "kind": "message", "as": "digest",
                      "row": 0, "level_before": "active", "level_after": "passive",
                      "badge_before": true, "badge_after": false}],
         "truncated": false}
        """
        let p = try JSON.decoder.decode(DeliveryPreview.self, from: Data(json.utf8))
        XCTAssertEqual(p.outcomes.silent, 10)
        XCTAssertEqual(p.row(0)?.refused, 2)
        XCTAssertNil(p.row(1))
        XCTAssertEqual(p.badgeLeft, 5)
        XCTAssertEqual(p.changed[0].applied, "digest")
        XCTAssertTrue(p.changed[0].badgeBefore)
    }

    func testDecodesSourcesAndOrdersNewFirst() throws {
        let json = """
        {"sources": [
          {"source": "backup", "first_seen": "2026-10-01T00:00:00Z", "last_seen": "2026-11-25T00:00:00Z",
           "reviewed_at": "2026-10-01T00:00:00Z", "new": false, "events": 40, "badge_in": 2, "badge_out": 38},
          {"source": "192.0.2.7", "first_seen": "2026-11-25T00:00:00Z", "last_seen": "2026-11-25T01:00:00Z",
           "new": true, "events": 3, "badge_in": 3, "badge_out": 0}
        ]}
        """
        let list = try JSON.decoder.decode(SourceList.self, from: Data(json.utf8))
        XCTAssertNil(list.sources[1].reviewedAt)
        XCTAssertEqual(Source.ordered(list.sources).map(\.source), ["192.0.2.7", "backup"])
        XCTAssertEqual(list.sources[0].badgeOut, 38)
    }

    // MARK: Input

    func testInputNormalizesAndLeavesOutEmptyFields() throws {
        let input = DeliverySettingInput(match: DeliveryMatch(source: " printer ", tag: "", level: [], key: "maint:"),
                                         deliver: .silent, badge: .auto, defaultLevel: "", position: 3, note: "  ")
        let json = String(decoding: try JSON.encoder.encode(input.normalized), as: UTF8.self)
        XCTAssertEqual(json, #"{"deliver":"silent","match":{"key":"maint:","source":"printer"},"position":3}"#)
    }

    func testInputKeepsEveryFieldOfTheRow() {
        let row = DeliverySetting(id: "dlv_a", match: DeliveryMatch(source: "x", project: "home"), deliver: .drop,
                                  badge: .off, position: 7, note: "n")
        XCTAssertEqual(DeliverySettingInput(row),
                       DeliverySettingInput(match: DeliveryMatch(source: "x", project: "home"), deliver: .drop,
                                            badge: .off, position: 7, note: "n"))
    }

    func testProblemsTheServerWouldRefuse() {
        XCTAssertNotNil(DeliverySettingInput(match: DeliveryMatch(source: "x")).problem)
        XCTAssertNotNil(DeliverySettingInput(badge: .auto).problem)
        XCTAssertNotNil(DeliverySettingInput(match: DeliveryMatch(level: ["active"]), defaultLevel: "passive").problem)
        XCTAssertNotNil(DeliverySettingInput(deliver: .digest, badge: .on).problem)
        XCTAssertNotNil(DeliverySettingInput(deliver: .drop, badge: .on).problem)
        XCTAssertNotNil(DeliverySettingInput(match: DeliveryMatch(kind: "progress"), deliver: .silent).problem)
        XCTAssertNil(DeliverySettingInput(match: DeliveryMatch(kind: "checkin"), deliver: .silent).problem)
        XCTAssertNil(DeliverySettingInput(deliver: .silent, badge: .on).problem)
        XCTAssertNil(DeliverySettingInput(badge: .off).problem)
        XCTAssertNil(DeliverySettingInput(defaultLevel: "passive").problem)
    }

    // MARK: Requests

    func testHiddenQueryAndHeader() async throws {
        let client = stubClient()
        XCTAssertEqual(EventQuery(hidden: .exclude).items.first { $0.name == "hidden" }?.value, "exclude")
        XCTAssertNil(EventQuery().items.first { $0.name == "hidden" })

        DeliveryStub.body = Self.mixedPage
        DeliveryStub.headers = ["Heraldo-Hidden": "only"]
        // A server with the filter: the page is kept as sent.
        var page = try await client.listEvents(EventQuery(hidden: .only))
        XCTAssertEqual(page.events.count, 3)
        XCTAssertTrue(DeliveryStub.requests.last?.url?.query?.contains("hidden=only") ?? false)

        // A server before the filter: the client drops the rows itself.
        DeliveryStub.headers = [:]
        page = try await client.listEvents(EventQuery(hidden: .exclude))
        XCTAssertEqual(page.events.map(\.id), ["shown"])
        XCTAssertEqual(page.fetchedCount, 3)
        page = try await client.listEvents(EventQuery(hidden: .only))
        XCTAssertEqual(page.events.map(\.id), ["filed", "dropped"])
    }

    func testCacheSyncAsksForHiddenExclude() async throws {
        let client = stubClient()
        DeliveryStub.body = #"{"events": [], "next_cursor": "c1"}"#
        let sync = HistorySync(client: client)
        _ = try await sync.sync(HistoryCache())
        XCTAssertFalse(DeliveryStub.requests.isEmpty)
        // headCursor asks with no filter; every cache page asks with hidden=exclude.
        let pages = DeliveryStub.requests.filter { $0.url?.query?.contains("routine=exclude") ?? false }
        XCTAssertFalse(pages.isEmpty)
        for r in pages { XCTAssertTrue(r.url?.query?.contains("hidden=exclude") ?? false, "\(r.url!)") }
    }

    func testStaleCacheFormat() throws {
        XCTAssertFalse(HistoryCache().isStale)
        let old = try JSON.decoder.decode(HistoryCache.self, from: Data(#"{"events": [], "reachedEnd": false}"#.utf8))
        XCTAssertTrue(old.isStale)
    }

    func testWritesUseTheAdminToken() async throws {
        var client = stubClient()
        DeliveryStub.body = Self.row
        do {
            _ = try await client.createDeliverySetting(DeliverySettingInput(deliver: .silent))
            XCTFail("wrote without an admin token")
        } catch let e as HeraldoError {
            XCTAssertEqual(e, .notConfigured("The admin token"))
        }
        client.adminToken = "adm"
        _ = try await client.createDeliverySetting(DeliverySettingInput(match: DeliveryMatch(source: "x"), deliver: .silent))
        var r = try XCTUnwrap(DeliveryStub.requests.last)
        XCTAssertEqual(r.httpMethod, "POST")
        XCTAssertEqual(r.url?.path, "/v1/delivery")
        XCTAssertEqual(r.value(forHTTPHeaderField: "Authorization"), "Bearer adm")
        XCTAssertEqual(DeliveryStub.bodies.last, #"{"deliver":"silent","match":{"source":"x"}}"#)

        _ = try await client.updateDeliverySetting(id: "dlv_a", DeliverySettingInput(badge: .off, position: 2))
        r = try XCTUnwrap(DeliveryStub.requests.last)
        XCTAssertEqual(r.httpMethod, "PUT")
        XCTAssertEqual(r.url?.path, "/v1/delivery/dlv_a")

        DeliveryStub.status = 204
        DeliveryStub.body = ""
        try await client.deleteDeliverySetting(id: "dlv_a")
        XCTAssertEqual(DeliveryStub.requests.last?.httpMethod, "DELETE")
    }

    func testPreviewUsesTheReadTokenAndNormalizes() async throws {
        let client = stubClient()
        DeliveryStub.body = """
        {"days": 7, "events": 0, "outcomes": {"banner": 0, "silent": 0, "digest": 0, "drop": 0, "unchanged": 0},
         "rows": [], "badge_joined": 0, "badge_left": 0, "changed": [], "truncated": false}
        """
        _ = try await client.previewDelivery(DeliveryPreviewInput(
            settings: [DeliverySettingInput(match: DeliveryMatch(source: ""), deliver: .drop, badge: .auto)], days: 7))
        let r = try XCTUnwrap(DeliveryStub.requests.last)
        XCTAssertEqual(r.url?.path, "/v1/delivery/preview")
        XCTAssertEqual(r.value(forHTTPHeaderField: "Authorization"), "Bearer r")
        XCTAssertEqual(DeliveryStub.bodies.last, #"{"days":7,"settings":[{"deliver":"drop","match":{}}]}"#)
    }

    func testReviewSourceEncodesTheNameOnce() async throws {
        var client = stubClient()
        client.adminToken = "adm"
        DeliveryStub.body = #"{"source": "a/b c%d", "new": false, "events": 0, "badge_in": 0, "badge_out": 0}"#
        _ = try await client.reviewSource("a/b c%d")
        let url = try XCTUnwrap(DeliveryStub.requests.last?.url)
        XCTAssertEqual(url.absoluteString, "https://heraldo.example/v1/sources/a%2Fb%20c%25d/review")
    }

    func testOldServerAnswersNil() async throws {
        let client = stubClient()
        DeliveryStub.status = 404
        DeliveryStub.body = #"{"error": "not found"}"#
        let rows = try await client.deliverySettings()
        let sources = try await client.sources()
        XCTAssertNil(rows)
        XCTAssertNil(sources)
    }

    func testViewFilterCarriesHidden() throws {
        let f = try JSON.decoder.decode(ViewFilter.self, from: Data(#"{"hidden": "only"}"#.utf8))
        XCTAssertEqual(f.hidden, .only)
        XCTAssertEqual(String(decoding: try JSON.encoder.encode(f), as: UTF8.self), #"{"hidden":"only"}"#)
    }

    // MARK: Fixtures

    static let mixedPage = """
    {"events": [
      {"id": "shown", "source": "s", "deliver": {"as": "silent", "badge": "auto"}},
      {"id": "filed", "source": "s", "deliver": {"as": "digest", "badge": "off"}},
      {"id": "dropped", "source": "s", "deliver": {"as": "drop", "badge": "auto"}}
    ], "next_cursor": "c3"}
    """

    static let row = """
    {"id": "dlv_a", "match": {"source": "x"}, "deliver": "silent", "badge": "auto", "position": 0,
     "created_at": "2026-11-20T10:00:00Z", "updated_at": "2026-11-20T10:00:00Z"}
    """

    private func stubClient() -> HeraldoClient {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [DeliveryStub.self]
        return HeraldoClient(baseURL: URL(string: "https://heraldo.example")!, readToken: "r",
                             session: URLSession(configuration: config))
    }
}

/// Answers every request with one body and records what was sent.
private final class DeliveryStub: URLProtocol {
    nonisolated(unsafe) static var headers: [String: String] = [:]
    nonisolated(unsafe) static var status = 200
    nonisolated(unsafe) static var body = ""
    nonisolated(unsafe) static var requests: [URLRequest] = []
    nonisolated(unsafe) static var bodies: [String] = []

    static func reset() {
        headers = [:]
        status = 200
        body = ""
        requests = []
        bodies = []
    }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        Self.requests.append(request)
        if let data = request.httpBody ?? request.httpBodyStream.map(Self.read) {
            Self.bodies.append(String(decoding: data, as: UTF8.self))
        }
        let response = HTTPURLResponse(url: request.url!, statusCode: Self.status, httpVersion: nil,
                                       headerFields: Self.headers.merging(["Content-Type": "application/json"]) { a, _ in a })!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data(Self.body.utf8))
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}

    private static func read(_ stream: InputStream) -> Data {
        stream.open()
        defer { stream.close() }
        var data = Data()
        var buf = [UInt8](repeating: 0, count: 4096)
        while stream.hasBytesAvailable {
            let n = stream.read(&buf, maxLength: buf.count)
            if n <= 0 { break }
            data.append(buf, count: n)
        }
        return data
    }
}
