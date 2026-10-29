import XCTest
@testable import HeraldoKit

/// Saved views: decoding, the request body and query, and the answer of a
/// server before views. The fixtures are hand-written.
final class ViewsTests: XCTestCase {
    func testDecodesViewList() throws {
        let json = """
        {"views": [{
          "id": "view_01JBA6Y8Q2K9M3T4V5W6X7Y8Z9", "name": "Backups",
          "filter": {"project": ["backups", "-"], "kind": "incident", "level": ["critical"], "routine": "exclude"},
          "bundle": {"window": "15m", "max_visible": 1}, "position": 0, "icon": "externaldrive",
          "created_at": "2026-10-26T12:00:00Z", "updated_at": "2026-10-26T12:10:00.5Z"
        }, {
          "id": "view_2", "name": "Plain", "filter": {}, "position": 3,
          "created_at": "2026-10-26T12:00:00Z", "updated_at": "2026-10-26T12:00:00Z"
        }]}
        """
        let list = try JSON.decoder.decode(ViewList.self, from: Data(json.utf8))
        XCTAssertEqual(list.views.count, 2)
        let v = list.views[0]
        XCTAssertEqual(v.name, "Backups")
        XCTAssertEqual(v.filter.project, ["backups", noProject])
        XCTAssertEqual(v.filter.level, ["critical"])
        XCTAssertEqual(v.filter.routine, .exclude)
        XCTAssertEqual(v.bundle, ViewBundle(window: "15m", maxVisible: 1))
        XCTAssertEqual(v.icon, "externaldrive")
        XCTAssertEqual(v.createdAt, JSON.parseDate("2026-10-26T12:00:00Z"))
        let plain = list.views[1]
        XCTAssertTrue(plain.filter.isEmpty)
        XCTAssertNil(plain.bundle)
        XCTAssertNil(plain.icon)
        XCTAssertEqual(plain.position, 3)
    }

    func testInputLeavesOutUnsetFields() throws {
        let input = ViewInput(name: "Ops", filter: ViewFilter(project: ["ops"], routine: .exclude))
        let json = String(decoding: try JSON.encoder.encode(input), as: UTF8.self)
        XCTAssertEqual(json, #"{"filter":{"project":["ops"],"routine":"exclude"},"name":"Ops"}"#)
    }

    func testQueryCarriesView() {
        let items = EventQuery(order: .desc, limit: 50, kind: "incident", view: "view_1").items
        XCTAssertEqual(items.first { $0.name == "view" }?.value, "view_1")
        XCTAssertEqual(items.first { $0.name == "kind" }?.value, "incident")
        XCTAssertNil(EventQuery().items.first { $0.name == "view" })
    }

    func testViewListNeedsTheViewHeader() async throws {
        let client = stubClient()
        StubProtocol.headers = ["Heraldo-View": "view_1"]
        let page = try await client.listEvents(EventQuery(view: "view_1"))
        XCTAssertEqual(page.events.count, 0)
        XCTAssertTrue(StubProtocol.lastURL?.query?.contains("view=view_1") ?? false)

        StubProtocol.headers = [:]
        do {
            _ = try await client.listEvents(EventQuery(view: "view_1"))
            XCTFail("a server without views must not answer for a view")
        } catch let e as HeraldoError {
            XCTAssertEqual(e, .unsupported("views"))
        }
        // Without a view the header does not matter.
        _ = try await client.listEvents(EventQuery())
    }

    func testViewsNilOnOldServer() async throws {
        let client = stubClient()
        StubProtocol.status = 404
        defer { StubProtocol.status = 200 }
        let views = try await client.views()
        XCTAssertNil(views)
    }

    private func stubClient() -> HeraldoClient {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [StubProtocol.self]
        return HeraldoClient(baseURL: URL(string: "https://heraldo.example")!, readToken: "r",
                             session: URLSession(configuration: config))
    }
}

/// Answers every request with an empty event page and `headers`.
private final class StubProtocol: URLProtocol {
    nonisolated(unsafe) static var headers: [String: String] = [:]
    nonisolated(unsafe) static var status = 200
    nonisolated(unsafe) static var lastURL: URL?

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        Self.lastURL = request.url
        let body = Self.status == 200 ? #"{"events":[]}"# : #"{"error":"not found"}"#
        let response = HTTPURLResponse(url: request.url!, statusCode: Self.status, httpVersion: nil,
                                       headerFields: Self.headers.merging(["Content-Type": "application/json"]) { a, _ in a })!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data(body.utf8))
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}
