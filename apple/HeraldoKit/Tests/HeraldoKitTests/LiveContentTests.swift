import XCTest
@testable import HeraldoKit

/// The Live Activity content model, against hand-written fixtures in the
/// shape the server pushes (internal/live/payload.go).
final class LiveContentTests: XCTestCase {
    func testDecodesPushedState() throws {
        let json = """
        {
          "title": "bracket.3mf", "symbol": "printer.fill", "tint": "#1E88E5", "body": "PLA, 0.2 mm",
          "progress": 0.42, "steps": {"total": 120, "current": 50}, "ends_at": 1793127300,
          "trailing": "1h 35m", "status": "Printing",
          "metrics": [{"label": "Nozzle", "value": "215", "unit": "°C", "color": "orange"}, {"label": "Bed"},
                      {"label": "Fan", "value": "80", "unit": "%"}],
          "button": {"id": "pause", "title": "Pause", "type": "http", "url": "http://192.0.2.20/pause"}
        }
        """
        let s = try JSONDecoder().decode(LiveState.self, from: Data(json.utf8))
        XCTAssertEqual(s.title, "bracket.3mf")
        XCTAssertEqual(s.endsAt, Date(timeIntervalSince1970: 1_793_127_300))
        XCTAssertEqual(s.metrics?.count, 2, "a metric without a value is dropped, not the state")
        XCTAssertEqual(s.metrics?.first?.text, "215 °C")
        XCTAssertEqual(s.metrics?.last?.text, "80%")
        XCTAssertEqual(s.button?.type, "http")
        XCTAssertEqual(s.fraction, 0.42)
        XCTAssertEqual(s.progressText, "50 of 120")
    }

    func testEmptyStateDecodes() throws {
        let s = try JSONDecoder().decode(LiveState.self, from: Data("{}".utf8))
        XCTAssertEqual(s, LiveState())
        XCTAssertNil(s.fraction)
        XCTAssertNil(s.progressText)
        XCTAssertTrue(s.shownMetrics.isEmpty)
    }

    func testStateRoundTrips() throws {
        let s = LiveState(title: "Backup", progress: 0.5, endsAt: Date(timeIntervalSince1970: 1_800_000_000),
                          status: "Copying", metrics: [LiveMetric(label: "Files", value: "1.2k")])
        let data = try JSONEncoder().encode(s)
        let raw = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
        XCTAssertEqual(raw["ends_at"] as? Double, 1_800_000_000, "ends_at stays Unix seconds")
        XCTAssertEqual(try JSONDecoder().decode(LiveState.self, from: data), s)
    }

    func testFraction() {
        XCTAssertEqual(LiveState(progress: 1.4).fraction, 1)
        XCTAssertEqual(LiveState(progress: -1).fraction, 0)
        XCTAssertEqual(LiveState(steps: LiveSteps(total: 4, current: 1)).fraction, 0.25)
        XCTAssertNil(LiveState(steps: LiveSteps(total: 0, current: 0)).fraction)
        XCTAssertEqual(LiveState(progress: 0.426).progressText, "43%")
    }

    func testShownMetricsCapAtSix() {
        let m = (1...8).map { LiveMetric(label: "M\($0)", value: "\($0)") }
        XCTAssertEqual(LiveState(metrics: m).shownMetrics.count, 6)
    }

    func testCountdown() {
        let now = Date(timeIntervalSince1970: 1000)
        XCTAssertEqual(LiveState(endsAt: Date(timeIntervalSince1970: 1600)).countdown(at: now),
                       now...Date(timeIntervalSince1970: 1600))
        XCTAssertNil(LiveState(endsAt: Date(timeIntervalSince1970: 900)).countdown(at: now))
        XCTAssertNil(LiveState().countdown(at: now))
    }

    func testTint() {
        XCTAssertEqual(LiveTint("Orange"), .named("orange"))
        XCTAssertEqual(LiveTint("#FF8000"), .rgba(red: 1, green: 128.0 / 255, blue: 0, alpha: 1))
        XCTAssertEqual(LiveTint("#00000080"), .rgba(red: 0, green: 0, blue: 0, alpha: 128.0 / 255))
        XCTAssertNil(LiveTint("#12345"))
        XCTAssertNil(LiveTint("chartreuse"))
        XCTAssertNil(LiveTint(nil))
    }

    func testPatchEncodesKeptClearedAndSet() throws {
        var p = LivePatch()
        XCTAssertTrue(p.isEmpty)
        p.progress = .set(0.5)
        p.trailing = .clear
        p.endsAt = .set(Date(timeIntervalSince1970: 1_800_000_000))
        p.metrics = .set([LiveMetric(label: "Bed", value: "60", unit: "°C")])
        p.keepFor = .set(600)
        let raw = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(p)) as? [String: Any])
        XCTAssertEqual(Set(raw.keys), ["progress", "trailing", "ends_at", "metrics", "keep_for"])
        XCTAssertTrue(raw["trailing"] is NSNull, "a cleared field is null")
        XCTAssertEqual(raw["ends_at"] as? String, "2027-01-15T08:00:00.000Z", "the envelope's ends_at is RFC 3339")
        XCTAssertEqual(raw["keep_for"] as? Int, 600)
    }

    func testEventInputCarriesContent() throws {
        var p = LivePatch()
        p.status = .set("Done")
        let input = EventInput(source: "shortcuts", kind: "progress", key: "k", action: "end", content: p)
        let raw = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(input)) as? [String: Any])
        XCTAssertEqual((raw["content"] as? [String: Any])?["status"] as? String, "Done")
        let plain = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(EventInput(source: "s"))) as? [String: Any])
        XCTAssertNil(plain["content"], "no content, no key")
    }

    func testMetricsText() {
        let m = LiveText.metrics("Nozzle: 215 °C, Bed=60 °C; no value:\nFan: 80%\n: x")
        XCTAssertEqual(m, [LiveMetric(label: "Nozzle", value: "215 °C"), LiveMetric(label: "Bed", value: "60 °C"),
                           LiveMetric(label: "Fan", value: "80%")])
        XCTAssertEqual(LiveText.metrics((1...9).map { "m\($0): \($0)" }.joined(separator: ",")).count, 6)
        XCTAssertTrue(LiveText.metrics("").isEmpty)
    }

    func testDecodesActivityInfo() throws {
        let json = """
        {"key": "print/42", "id": "act_01J", "state": "active", "source": "printer", "content": {},
         "started_at": "2026-10-27T09:00:00Z", "updated_at": "2026-10-27T09:10:00.5Z",
         "instances": [{"id": "lai_01J", "device": "dev_01", "chain": 0, "state": "active",
                        "requested_at": "2026-10-27T09:00:00Z"}]}
        """
        let a = try JSON.decoder.decode(LiveActivityInfo.self, from: Data(json.utf8))
        XCTAssertEqual(a.instances.first?.state, "active")
        XCTAssertNotNil(a.updatedAt)
    }
}

/// The Shortcut fields, mapped to envelope content.
final class LiveFieldsTests: XCTestCase {
    func testStartFields() throws {
        var f = LiveFields()
        f.title = "  Backup  "
        f.progress = 42
        f.endsIn = 600
        f.steps = 10
        f.step = 12
        f.tint = "chartreuse"
        f.metricsText = "Files: 1.2k"
        f.buttonTitle = "Open"
        f.buttonURL = "https://example.com/job"
        let now = Date(timeIntervalSince1970: 1_800_000_000)
        let p = f.patch(now: now)
        XCTAssertEqual(p.title, .set("Backup"))
        XCTAssertEqual(p.progress, .set(0.42))
        XCTAssertEqual(p.endsAt, .set(now.addingTimeInterval(600)))
        XCTAssertEqual(p.steps, .set(LiveSteps(total: 10, current: 10)), "step is capped at steps")
        XCTAssertEqual(p.tint, .keep, "an unknown color is left out, not sent for the server to refuse")
        XCTAssertEqual(p.metrics, .set([LiveMetric(label: "Files", value: "1.2k")]))
        XCTAssertEqual(p.button, .set(LiveButton(id: "button", title: "Open", type: "open", url: "https://example.com/job")))
    }

    func testHTTPButton() {
        var f = LiveFields()
        f.buttonTitle = "Pause"
        f.buttonURL = "http://192.0.2.20/pause"
        f.buttonOpensLink = false
        XCTAssertEqual(f.patch().button, .set(LiveButton(id: "button", title: "Pause", type: "http",
                                                          url: "http://192.0.2.20/pause", method: "POST")))
    }

    func testClearFields() {
        var f = LiveFields()
        f.status = "Paused"
        let p = f.patch(clearing: [.countdown, .status, .metrics, .stages, .button, .trailing])
        XCTAssertEqual(p.endsAt, .clear)
        XCTAssertEqual(p.status, .set("Paused"), "a field set in the same call is not cleared")
        XCTAssertEqual(p.metrics, .clear)
        XCTAssertEqual(p.steps, .clear)
        XCTAssertEqual(p.button, .clear)
        XCTAssertEqual(p.trailing, .clear)
        XCTAssertTrue(LiveFields().patch().isEmpty)
    }
}
