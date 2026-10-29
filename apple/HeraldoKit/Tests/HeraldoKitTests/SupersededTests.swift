import XCTest
@testable import HeraldoKit

final class SupersededTests: XCTestCase {
    private func info(_ id: String, key: String?, action: String? = nil) -> [AnyHashable: Any] {
        var h: [String: Any] = ["id": id, "kind": "incident", "source": "uptime"]
        if let key { h["key"] = key }
        if let action { h["action"] = action }
        return ["aps": ["alert": ["title": "t"]], "heraldo": h]
    }

    private func push(_ id: String, key: String?, action: String? = nil) -> PushInfo {
        PushInfo(userInfo: info(id, key: key, action: action))!
    }

    func testResolveRemovesTheOpenOfItsKey() {
        let delivered = [
            Superseded.Delivered(identifier: "bundle-1", userInfo: info("01A", key: "web", action: "open")),
            Superseded.Delivered(identifier: "n2", userInfo: info("01B", key: "db", action: "open")),
            Superseded.Delivered(identifier: "n3", userInfo: info("01C", key: nil)),
            Superseded.Delivered(identifier: "n4", userInfo: ["aps": [:]]),
            Superseded.Delivered(identifier: "n5", userInfo: info("01D", key: "web", action: "update")),
        ]
        let ids = Superseded.identifiers(delivered, for: push("01E", key: "web", action: "resolve"))
        XCTAssertEqual(ids, ["bundle-1", "n5"])
    }

    func testNeverRemovesItself() {
        let delivered = [
            Superseded.Delivered(identifier: "c", userInfo: info("01E", key: "web")),
            Superseded.Delivered(identifier: "same", userInfo: info("01A", key: "web")),
            Superseded.Delivered(identifier: "old", userInfo: info("01B", key: "web")),
        ]
        let p = push("01E", key: "web", action: "resolve")
        XCTAssertEqual(Superseded.identifiers(delivered, for: p, keeping: "same"), ["old"],
                       "the same event id stays, and so does the push's own identifier")
    }

    func testAPushWithoutAKeyRemovesNothing() {
        let delivered = [Superseded.Delivered(identifier: "a", userInfo: info("01A", key: "web"))]
        XCTAssertEqual(Superseded.identifiers(delivered, for: push("01B", key: nil)), [])
        XCTAssertEqual(Superseded.identifiers(delivered, for: push("01B", key: "")), [])
    }
}
