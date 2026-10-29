import UniformTypeIdentifiers
import XCTest
@testable import HeraldoKit

/// Share in and share out.
final class ShareTests: XCTestCase {
    /// The same two cases as TestMarkdown in internal/client/markdown_test.go:
    /// the apps and `heraldo show --md` must share one text.
    func testShareMarkdownMatchesTheServerText() throws {
        let cases: [(String, String)] = [
            ("""
            {"id":"01JAAAAAAAAAAAAAAAAAAAAAAA","source":"watch","kind":"incident","action":"open","key":"disk:a",
             "host":"box1","level":"time-sensitive","project":"infra","title":"Disk a full","summary":"98% used",
             "body":"Free space is low.\\n\\nClean up.","url":"https://example.com/disk","tags":["disk","ops"],
             "ts":"2026-10-29T07:30:00.123Z","received_at":"2026-10-29T07:30:01Z",
             "attachment_info":[{"id":"x","content_type":"image/png","bytes":10},{"id":"y","content_type":"text/plain","bytes":3}]}
            """,
             "# Disk a full\n\n98% used\n\nFree space is low.\n\nClean up.\n\n" +
                "- Source: watch\n- Host: box1\n- Kind: incident open\n- Key: disk:a\n- Level: time-sensitive\n" +
                "- Project: infra\n- Time: 2026-10-29T07:30:00Z\n- Link: https://example.com/disk\n- Tags: disk, ops\n" +
                "- Attachments: 2 (image/png, text/plain)\n\nheraldo://event/01JAAAAAAAAAAAAAAAAAAAAAAA\n"),
            ("""
            {"id":"01JBBBBBBBBBBBBBBBBBBBBBBB","source":"share:iphone","kind":"message","summary":"Line one\\nline two",
             "ts":"2026-10-29T08:00:00Z"}
            """,
             "# Line one line two\n\n- Source: share:iphone\n- Level: active\n" +
                "- Time: 2026-10-29T08:00:00Z\n\nheraldo://event/01JBBBBBBBBBBBBBBBBBBBBBBB\n"),
        ]
        for (json, want) in cases {
            let e = try JSON.decoder.decode(Event.self, from: Data(json.utf8))
            XCTAssertEqual(e.shareMarkdown, want)
        }
    }

    func testEventIDFromLink() {
        XCTAssertEqual(eventID(fromLinkOrID: "heraldo://event/01JAAAAAAAAAAAAAAAAAAAAAAA"), "01JAAAAAAAAAAAAAAAAAAAAAAA")
        XCTAssertEqual(eventID(fromLinkOrID: " 01JAAAAAAAAAAAAAAAAAAAAAAA\n"), "01JAAAAAAAAAAAAAAAAAAAAAAA")
        XCTAssertNil(eventID(fromLinkOrID: "https://example.com/x"))
        XCTAssertNil(eventID(fromLinkOrID: ""))
    }

    func testSourceSlug() {
        XCTAssertEqual(ShareSender.source(device: "iPhone"), "share:iphone")
        XCTAssertEqual(ShareSender.source(device: "Office Mac mini (2)"), "share:office-mac-mini-2")
        XCTAssertEqual(ShareSender.source(device: "  ✨ "), "share:device")
    }

    func testDraftToMessage() throws {
        var d = ShareDraft(text: "Look at this\nsecond line", url: URL(string: "https://example.com/a")!,
                           project: "reading")
        d.dedup = "share-1"
        let e = d.input(source: "share:iphone", attachmentIDs: ["att1"])
        XCTAssertEqual(e.title, "Look at this")
        XCTAssertEqual(e.body, "Look at this\nsecond line\n\nhttps://example.com/a")
        XCTAssertEqual(e.url, "https://example.com/a")
        XCTAssertEqual(e.level, "passive")
        XCTAssertEqual(e.project, "reading")
        XCTAssertNil(e.kind)
        XCTAssertEqual(e.attachments, ["att1"])
        let json = String(decoding: try JSONEncoder().encode(e), as: UTF8.self)
        XCTAssertTrue(json.contains("\"dedup\":\"share-1\""), json)
        XCTAssertFalse(json.contains("\"key\""), json)
    }

    func testDraftToIncidentUpdate() {
        let d = ShareDraft(title: "Printer log", files: [], project: "home", incidentKey: "printer:jam")
        let e = d.input(source: "share:mac", attachmentIDs: [])
        XCTAssertEqual(e.kind, "incident")
        XCTAssertEqual(e.action, "update")
        XCTAssertEqual(e.key, "printer:jam")
        XCTAssertNil(e.project, "the incident's key files it")
        XCTAssertEqual(e.level, "passive")
        XCTAssertNil(e.attachments)
    }

    func testDraftTitleFallbacks() {
        XCTAssertEqual(ShareDraft(url: URL(string: "https://example.com/a")!).resolvedTitle, "example.com")
        let f = ShareFile(name: "shot.png", data: Data([1]), contentType: "image/png")
        XCTAssertEqual(ShareDraft(files: [f]).resolvedTitle, "shot.png")
        XCTAssertThrowsError(try ShareDraft().checked()) { XCTAssertEqual($0 as? ShareError, .nothingToSend) }
    }

    func testLongTextBecomesAFile() throws {
        let long = String(repeating: "x", count: ShareLimits.maxBodyBytes + 1)
        let d = try ShareDraft(text: long).checked()
        XCTAssertEqual(d.files.first?.contentType, "text/plain")
        XCTAssertEqual(d.files.first?.data.count, long.utf8.count)
        XCTAssertLessThanOrEqual(d.resolvedBody.utf8.count, ShareLimits.maxBodyBytes)
    }

    func testFileTypes() throws {
        XCTAssertEqual(ShareLimits.contentType(for: .png), "image/png")
        XCTAssertEqual(ShareLimits.contentType(for: .pdf), "application/pdf")
        XCTAssertEqual(ShareLimits.contentType(for: .json), "application/json")
        XCTAssertEqual(ShareLimits.contentType(for: .plainText), "text/plain")
        XCTAssertNil(ShareLimits.contentType(for: .svg))
        XCTAssertNil(ShareLimits.contentType(for: .zip))
        XCTAssertThrowsError(try ShareFile.make(name: "a.zip", data: Data([1]), type: .zip))
        XCTAssertThrowsError(try ShareFile.make(name: "big.pdf", data: Data(count: ShareLimits.maxBytes + 1), type: .pdf)) {
            guard case .tooLarge = $0 as? ShareError else { return XCTFail("\($0)") }
        }
        let many = Array(repeating: ShareFile(name: "a", data: Data([1]), contentType: "text/plain"), count: 9)
        XCTAssertThrowsError(try ShareDraft(files: many).checked())
    }

    func testPrefsRememberTheProject() {
        let suite = "heraldo.tests.\(UUID().uuidString)"
        defer { UserDefaults().removePersistentDomain(forName: suite) }
        let p = SharePrefs(suite: suite)
        XCTAssertNil(p.lastProject)
        p.lastProject = "infra"
        XCTAssertEqual(SharePrefs(suite: suite).lastProject, "infra")
    }

    func testEventInputEncodesNewFields() throws {
        let e = EventInput(source: "s", project: "p", url: "https://example.com", attachments: ["a"], dedup: "d")
        let back = try JSONDecoder().decode(EventInput.self, from: try JSONEncoder().encode(e))
        XCTAssertEqual(back, e)
    }
}
