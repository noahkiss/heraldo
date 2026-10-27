import Security
import XCTest
@testable import HeraldoKit

/// The Keychain query dictionaries. `swift test` has no Keychain entitlement,
/// so these check the queries, not the items.
final class CredentialStoreTests: XCTestCase {
    let group = "group.com.example.heraldo"

    func testSharedQuerySyncsInTheGroup() {
        let q = CredentialStore(mode: .shared(accessGroup: group)).baseQuery()
        XCTAssertEqual(q[kSecClass as String] as? String, kSecClassGenericPassword as String)
        XCTAssertEqual(q[kSecAttrService as String] as? String, CredentialStore.service)
        XCTAssertEqual(q[kSecAttrAccount as String] as? String, CredentialStore.account)
        XCTAssertEqual(q[kSecUseDataProtectionKeychain as String] as? Bool, true)
        XCTAssertEqual(q[kSecAttrAccessGroup as String] as? String, group)
        XCTAssertEqual(q[kSecAttrSynchronizable as String] as? Bool, true,
                       "every query, delete included, names the synced item")
        XCTAssertNil(q[kSecAttrAccessible as String])
    }

    func testSharedAddIsSyncableAfterFirstUnlock() {
        let q = CredentialStore(mode: .shared(accessGroup: group)).addQuery(data: Data("{}".utf8))
        XCTAssertEqual(q[kSecAttrSynchronizable as String] as? Bool, true)
        XCTAssertEqual(q[kSecAttrAccessible as String] as? String, kSecAttrAccessibleAfterFirstUnlock as String,
                       "a ThisDeviceOnly class would not sync")
        XCTAssertEqual(q[kSecValueData as String] as? Data, Data("{}".utf8))
        XCTAssertEqual(q[kSecAttrAccessGroup as String] as? String, group)
    }

    func testLegacyQueriesAreDeviceOnly() {
        let qs = CredentialStore(mode: .shared(accessGroup: group)).legacyQueries("read-token")
        XCTAssertEqual(qs.count, 2, "the app group first, then the app's own group")
        XCTAssertEqual(qs[0][kSecAttrAccessGroup as String] as? String, group)
        XCTAssertNil(qs[1][kSecAttrAccessGroup as String])
        for q in qs {
            XCTAssertEqual(q[kSecAttrService as String] as? String, "heraldo")
            XCTAssertEqual(q[kSecAttrAccount as String] as? String, "read-token")
            XCTAssertNil(q[kSecAttrSynchronizable as String], "the old items never synced")
        }
    }

    func testModeFromInfoPlist() {
        XCTAssertEqual(CredentialStore.mode(group: " \(group) "), .shared(accessGroup: group))
        #if os(macOS)
        XCTAssertEqual(CredentialStore.mode(group: "$(APP_GROUP_ID)"), .local)
        XCTAssertEqual(CredentialStore.mode(group: nil), .local)
        let q = CredentialStore(mode: .local).baseQuery()
        XCTAssertEqual(q[kSecUseDataProtectionKeychain as String] as? Bool, false)
        XCTAssertNil(q[kSecAttrSynchronizable as String], "a local item does not sync")
        #endif
    }

    func testPayloadRoundTrips() throws {
        let c = Credentials(serverURL: "https://example.com", readToken: "r", writeToken: nil)
        let data = try JSONEncoder().encode(c)
        let obj = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
        XCTAssertEqual(obj["server_url"] as? String, "https://example.com")
        XCTAssertEqual(try JSONDecoder().decode(Credentials.self, from: data), c)
    }
}
