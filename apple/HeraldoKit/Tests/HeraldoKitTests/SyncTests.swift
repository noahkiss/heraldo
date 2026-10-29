import XCTest
@testable import HeraldoKit

/// The stream parser, the revision vector and the widget snapshot's
/// per-event update, from hand-written fixtures.
final class StreamParserTests: XCTestCase {
    private func parse(_ text: String) -> [SSEOutput] {
        var p = SSEParser()
        return p.feed(Array(text.utf8))
    }

    func testParsesTodaysStream() {
        // What a server before revisions sends: retry, a poke, a keep-alive.
        let out = parse("retry: 3000\n\nevent: poke\ndata: {\"cursor\":\"c1\"}\n\n: keep-alive\n\n")
        XCTAssertEqual(out, [
            .event(SSEEvent(event: "poke", data: #"{"cursor":"c1"}"#)),
            .comment("keep-alive"),
        ])
        guard case .event(let e) = out[0], case .poke(let poke)? = StreamMessage(e) else {
            return XCTFail("no poke")
        }
        XCTAssertEqual(poke.cursor, "c1")
        XCTAssertNil(poke.revisions)
    }

    func testRetryIsKept() {
        var p = SSEParser()
        _ = p.feed(Array("retry: 3000\n\n".utf8))
        XCTAssertEqual(p.retry, 3000)
    }

    func testLineEndingsAndMultilineData() {
        let out = parse("event: x\r\ndata: a\rdata:b\r\n\r\ndata: c\n\n")
        XCTAssertEqual(out, [.event(SSEEvent(event: "x", data: "a\nb")), .event(SSEEvent(data: "c"))])
    }

    func testBytesSplitAcrossFeeds() {
        var p = SSEParser()
        var out: [SSEOutput] = []
        for chunk in ["eve", "nt: poke\nda", "ta: {}", "\n", "\n"] { out += p.feed(Array(chunk.utf8)) }
        XCTAssertEqual(out, [.event(SSEEvent(event: "poke", data: "{}"))])
    }

    func testEventWithoutDataDispatchesNothing() {
        XCTAssertEqual(parse("event: poke\n\n"), [])
        // The event name does not leak into the next event.
        XCTAssertEqual(parse("event: poke\n\ndata: x\n\n"), [.event(SSEEvent(data: "x"))])
    }

    func testRevisionsAndRichPoke() {
        let out = parse("""
        event: revisions
        data: {"boot":"b1","rev":{"events":4,"cursor":2,"future":7}}

        event: poke
        data: {"cursor":"c9","boot":"b1","rev":{"events":5,"cursor":2}}


        """)
        XCTAssertEqual(out.count, 2)
        guard case .event(let r) = out[0], case .revisions(let v)? = StreamMessage(r) else { return XCTFail("no revisions") }
        XCTAssertEqual(v, Revisions(boot: "b1", rev: ["events": 4, "cursor": 2, "future": 7]))
        guard case .event(let pe) = out[1], case .poke(let p)? = StreamMessage(pe) else { return XCTFail("no poke") }
        XCTAssertEqual(p.cursor, "c9")
        XCTAssertEqual(p.revisions, Revisions(boot: "b1", rev: ["events": 5, "cursor": 2]))
    }

    func testOtherEventsAreIgnored() {
        XCTAssertNil(StreamMessage(SSEEvent(event: "message", data: "{}")))
        XCTAssertNil(StreamMessage(SSEEvent(event: "revisions", data: "not json")))
        // A poke that does not parse still pokes.
        XCTAssertEqual(StreamMessage(SSEEvent(event: "poke", data: "x")), .poke(Poke()))
    }

    func testStreamRequest() throws {
        let client = HeraldoClient(baseURL: URL(string: "https://example.test/base")!, readToken: "r")
        let r = client.streamRequest(heartbeat: 500)
        XCTAssertEqual(r.url?.absoluteString, "https://example.test/base/v1/stream?heartbeat=120")
        XCTAssertEqual(r.value(forHTTPHeaderField: "Accept"), "text/event-stream")
        XCTAssertEqual(r.timeoutInterval, 250)
        XCTAssertEqual(client.streamRequest(heartbeat: 1).url?.query, "heartbeat=15")
    }

    func testBackoff() {
        let b = Backoff()
        XCTAssertEqual(b.delay(attempt: 0, unit: 0), 1.5)
        XCTAssertEqual(b.delay(attempt: 0, unit: 1), 3)
        XCTAssertEqual(b.delay(attempt: 3, unit: 1), 24)
        XCTAssertEqual(b.delay(attempt: 10, unit: 1), 300)
        XCTAssertEqual(b.delay(attempt: 10, unit: 0), 150)
        for n in 0..<30 { XCTAssertLessThanOrEqual(b.delay(attempt: n), 300) }
    }
}

final class RevisionsTests: XCTestCase {
    private let all: [String: Int64] = ["events": 1, "cursor": 1, "state": 1, "checks": 1, "projects": 1, "views": 1,
                                        "held": 1, "activities": 1, "llm": 1, "mutes": 1, "settings": 1]

    func testFirstVectorAndNewBootAreFull() {
        let v = Revisions(boot: "a", rev: all)
        XCTAssertEqual(v.plan(since: nil), .full)
        XCTAssertEqual(v.plan(since: Revisions(boot: "b", rev: all)), .full)
    }

    func testSameVectorIsEmpty() {
        let v = Revisions(boot: "a", rev: all)
        XCTAssertTrue(v.plan(since: v).isEmpty)
        XCTAssertTrue(v.plan(since: v, poke: true).isEmpty)
    }

    func testChangedDomainsMapToParts() {
        let old = Revisions(boot: "a", rev: all)
        var rev = all
        rev["cursor"] = 2
        XCTAssertEqual(Revisions(boot: "a", rev: rev).plan(since: old), .parts([.unread]))
        rev["events"] = 9
        rev["held"] = 3
        XCTAssertEqual(Revisions(boot: "a", rev: rev).plan(since: old), .parts([.unread, .history, .held]))
        var s = all
        s["settings"] = 5
        XCTAssertEqual(Revisions(boot: "a", rev: s).plan(since: old), .parts([.state, .unread]))
    }

    func testUnknownDomainsAreIgnored() {
        let old = Revisions(boot: "a", rev: all.merging(["future": 1]) { $1 })
        let new = Revisions(boot: "a", rev: all.merging(["future": 2]) { $1 })
        XCTAssertTrue(new.plan(since: old).isEmpty)
    }

    func testMissingDomainRefreshesOnAnyPoke() {
        var partial = all
        partial["views"] = nil
        let old = Revisions(boot: "a", rev: partial)
        XCTAssertTrue(old.plan(since: old).isEmpty)
        XCTAssertEqual(old.plan(since: old, poke: true), .parts([.views]))
        var next = partial
        next["checks"] = 2
        XCTAssertEqual(Revisions(boot: "a", rev: next).plan(since: old), .parts([.checks, .views]))
    }

    func testDecodesLeniently() throws {
        let v = try JSON.decoder.decode(Revisions.self, from: Data(#"{"boot":"b","rev":{"events":3,"odd":"x","cursor":12}}"#.utf8))
        XCTAssertEqual(v.rev, ["events": 3, "cursor": 12])
    }

    func testRequestsMerge() {
        var r = SyncRequest(vector: Revisions(boot: "a", rev: ["events": 1]))
        r.merge(SyncRequest(poke: true, parts: [.checks]))
        r.merge(SyncRequest(vector: Revisions(boot: "a", rev: ["events": 2])))
        XCTAssertEqual(r, SyncRequest(vector: Revisions(boot: "a", rev: ["events": 2]), poke: true, parts: [.checks]))
        XCTAssertTrue(SyncRequest().isEmpty)
    }
}

final class WidgetApplyTests: XCTestCase {
    private func event(_ id: String, kind: String, key: String? = nil, action: String? = nil,
                       project: String? = "home", title: String = "t") -> Event {
        var e = Event(id: id, source: "s", kind: kind, title: title)
        e.key = key
        e.action = action
        e.project = project
        e.ts = Date(timeIntervalSince1970: Double(id.hashValue & 0xFFFF))
        return e
    }

    func testIncidentOpenAckResolve() {
        var s = WidgetSnapshot(updated: .distantPast)
        let kinds = s.apply(event("e1", kind: "incident", key: "disk", action: "open"), at: Date(timeIntervalSince1970: 5))
        XCTAssertEqual(kinds, [WidgetKinds.attention, WidgetKinds.project])
        XCTAssertEqual(s.incidents.map(\.key), ["disk"])
        XCTAssertEqual(s.updated, Date(timeIntervalSince1970: 5))
        // An update replaces the line.
        s.apply(event("e2", kind: "incident", key: "disk", action: "update", title: "worse"))
        XCTAssertEqual(s.incidents.map(\.id), ["e2"])
        // An ack removes it.
        s.apply(event("e3", kind: "incident", key: "disk", action: "ack"))
        XCTAssertTrue(s.incidents.isEmpty)
    }

    func testSameEventTwiceChangesNothing() {
        var s = WidgetSnapshot(updated: .distantPast)
        let e = event("e1", kind: "message")
        XCTAssertEqual(s.apply(e), [WidgetKinds.project])
        let at = s.updated
        XCTAssertEqual(s.apply(e), [])
        XCTAssertEqual(s.updated, at)
    }

    func testCheckDownAndRecovered() {
        var s = WidgetSnapshot()
        var fail = event("c1", kind: "checkin", key: "backup", action: "fail")
        fail.project = nil
        XCTAssertEqual(s.apply(fail), [WidgetKinds.attention, WidgetKinds.project])
        XCTAssertEqual(s.down.map(\.id), ["backup"])
        s.apply(event("r1", kind: "incident", key: "checkin:backup", action: "resolve", project: nil))
        XCTAssertTrue(s.down.isEmpty)
    }

    func testRecentKeepsThreePerProject() {
        var s = WidgetSnapshot()
        for i in 0..<5 { s.apply(event("m\(i)", kind: "message", project: "a")) }
        s.apply(event("x", kind: "message", project: "b"))
        XCTAssertEqual(s.recent.map(\.id), ["x", "m4", "m3", "m2"])
        XCTAssertEqual(s.projects, ["a", "b"])
        // Routine events stay out.
        XCTAssertEqual(s.apply(event("p", kind: "progress", project: "a")), [])
    }

    func testOldSnapshotDecodesWithoutKey() throws {
        let json = #"{"updated":"2026-10-01T00:00:00Z","incidents":[{"id":"i","title":"t"}],"down":[],"projects":[],"recent":[]}"#
        let s = try JSON.decoder.decode(WidgetSnapshot.self, from: Data(json.utf8))
        XCTAssertNil(s.incidents.first?.key)
    }
}

/// Answers every request with one canned stream body, then ends it.
final class StubStreamProtocol: URLProtocol {
    nonisolated(unsafe) static var body = ""
    nonisolated(unsafe) static var seen: [URLRequest] = []
    private static let lock = NSLock()

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        Self.lock.lock()
        Self.seen.append(request)
        Self.lock.unlock()
        let response = HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1",
                                       headerFields: ["Content-Type": "text/event-stream"])!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data(Self.body.utf8))
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}

final class EventStreamTests: XCTestCase {
    func testStreamDeliversMessagesThenBacksOff() async throws {
        StubStreamProtocol.body = """
        retry: 3000

        event: revisions
        data: {"boot":"b","rev":{"events":1}}

        : keep-alive

        event: poke
        data: {"cursor":"c2","boot":"b","rev":{"events":2}}


        """
        let got = expectation(description: "messages")
        let failed = expectation(description: "backoff")
        failed.assertForOverFulfill = false
        let box = Box()
        let client = HeraldoClient(baseURL: URL(string: "https://example.test")!, readToken: "r")
        let stream = EventStream(client: client, heartbeat: 60, backoff: Backoff(base: 30, cap: 60), onMessage: { m in
            if box.append(m) == 3 { got.fulfill() }
        }, onStatus: { s in
            // The stream had opened, so the first retry waits from the base.
            if case .failed(let wait) = s, wait >= 15, wait <= 30 { failed.fulfill() }
        }, configure: { $0.protocolClasses = [StubStreamProtocol.self] })
        stream.start()
        await fulfillment(of: [got, failed], timeout: 5)
        stream.stop()
        XCTAssertEqual(box.messages, [
            .revisions(Revisions(boot: "b", rev: ["events": 1])),
            .keepAlive,
            .poke(Poke(cursor: "c2", revisions: Revisions(boot: "b", rev: ["events": 2]))),
        ])
        XCTAssertEqual(StubStreamProtocol.seen.first?.url?.query, "heartbeat=60")
        XCTAssertEqual(StubStreamProtocol.seen.first?.value(forHTTPHeaderField: "Authorization"), "Bearer r")
    }

    final class Box: @unchecked Sendable {
        private let lock = NSLock()
        private(set) var messages: [StreamMessage] = []
        func append(_ m: StreamMessage) -> Int {
            lock.lock()
            defer { lock.unlock() }
            messages.append(m)
            return messages.count
        }
    }
}
