import XCTest
@testable import HeraldoKit

/// The activity list and the hold queue, from hand-written fixtures.
final class HomesTests: XCTestCase {
    func testDecodesActivities() throws {
        let json = """
        {"activities": [{
          "key": "job/build-1", "id": "act_01JBA6Y8Q2K9M3T4V5W6X7Y8Z9", "state": "active", "source": "ci",
          "content": {"title": "Build", "progress": 0.4, "ends_at": "2026-10-26T13:00:00Z",
                      "metrics": [{"label": "Step", "value": "3"}, {"bad": true}]},
          "started_at": "2026-10-26T12:00:00Z", "updated_at": "2026-10-26T12:10:00Z",
          "instances": [{"id": "lai_1", "device": "dev_1", "chain": 0, "state": "active", "requested_at": "2026-10-26T12:00:00Z"}]
        }, {
          "key": "job/plain", "id": "act_2", "state": "active", "source": "ci", "content": {},
          "started_at": "2026-10-26T12:00:00Z", "updated_at": "2026-10-26T12:00:00Z", "instances": []
        }]}
        """
        let list = try JSON.decoder.decode(ActivityList.self, from: Data(json.utf8))
        XCTAssertEqual(list.activities.count, 2)
        let a = list.activities[0]
        XCTAssertEqual(a.id, "job/build-1")
        XCTAssertEqual(a.activityID, "act_01JBA6Y8Q2K9M3T4V5W6X7Y8Z9")
        XCTAssertEqual(a.headline, "Build")
        XCTAssertEqual(a.content?.endsAt, JSON.parseDate("2026-10-26T13:00:00Z"))
        XCTAssertEqual(a.content?.metrics?.map(\.label), ["Step"])
        XCTAssertEqual(a.content?.state.progressText, "40%")
        XCTAssertEqual(list.activities[1].headline, "job/plain")
    }

    func testDecodesHoldQueue() throws {
        let json = """
        {"mode": "on", "last_poll_ts": null, "deadman_active": false, "held": [
          {"id": "h-1", "event_id": "01JBA6Y8Q2K9M3T4V5W6X7Y8Z9", "ts": 1761480000, "deadline_ts": 1761481800,
           "title": "Disk full", "source": "monitor", "forwarded": false, "shadow": false, "status": "held"},
          {"id": "h-2", "ts": 1761470000.5, "deadline_ts": 1761471800, "body": "line one\\nline two",
           "forwarded": true, "shadow": false, "status": "released", "verdict": "BLIP"}
        ]}
        """
        let q = try JSON.decoder.decode(HoldQueue.self, from: Data(json.utf8))
        XCTAssertEqual(q.mode, "on")
        XCTAssertEqual(q.held.map(\.isOpen), [true, false])
        XCTAssertEqual(q.held[0].headline, "Disk full")
        XCTAssertEqual(q.held[1].headline, "line one")
        XCTAssertEqual(q.held[0].deadline, Date(timeIntervalSince1970: 1761481800))
    }
}
