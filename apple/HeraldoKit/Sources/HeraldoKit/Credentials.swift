import Foundation
import Security

/// The server URL and the token pair the person pastes into Settings.
public struct Credentials: Codable, Equatable, Sendable {
    public var serverURL: String?
    public var readToken: String?
    public var writeToken: String?

    enum CodingKeys: String, CodingKey {
        case serverURL = "server_url"
        case readToken = "read_token"
        case writeToken = "write_token"
    }

    public init(serverURL: String? = nil, readToken: String? = nil, writeToken: String? = nil) {
        self.serverURL = serverURL
        self.readToken = readToken
        self.writeToken = writeToken
    }

    public var isComplete: Bool {
        !(serverURL ?? "").isEmpty && !(readToken ?? "").isEmpty
    }
}

/// Keeps the credentials in the Keychain as one item.
///
/// Two modes, picked by whether the bundle names an access group:
///
/// - **Shared** (an access group is set): a data-protection keychain item in
///   the app group, with `kSecAttrSynchronizable`, so iCloud Keychain carries
///   it to the person's other devices. The notification service extension
///   reads it through the same group.
/// - **Local** (macOS only, no group): a login-keychain item for this Mac. It
///   does not sync.
public struct CredentialStore: Sendable {
    public static let service = "heraldo.credentials"
    public static let account = "default"
    /// The Info.plist key the build fills with the app group.
    public static let accessGroupInfoKey = "HeraldoAppGroup"

    public enum Mode: Sendable, Equatable {
        case shared(accessGroup: String)
        #if os(macOS)
        case local
        #endif
    }

    public let mode: Mode

    public init(mode: Mode) {
        self.mode = mode
    }

    /// The store the running bundle's Info.plist asks for.
    public init(bundle: Bundle = .main) {
        self.init(mode: Self.mode(group: bundle.object(forInfoDictionaryKey: Self.accessGroupInfoKey) as? String))
    }

    static func mode(group raw: String?) -> Mode {
        let group = raw?.trimmingCharacters(in: .whitespaces) ?? ""
        if !group.isEmpty && !group.hasPrefix("$(") { return .shared(accessGroup: group) }
        #if os(macOS)
        return .local
        #else
        // iOS has no login keychain; an iOS build must name a group.
        return .shared(accessGroup: group)
        #endif
    }

    public var modeDescription: String {
        switch mode {
        case .shared: return "iCloud Keychain"
        #if os(macOS)
        case .local: return "This Mac"
        #endif
        }
    }

    /// The stored credentials; empty when there are none or the Keychain refuses.
    public func load() -> Credentials {
        (try? read()) ?? Credentials()
    }

    public func read() throws -> Credentials? {
        var q = baseQuery()
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        let status = SecItemCopyMatching(q as CFDictionary, &out)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess, let data = out as? Data else { throw KeychainError(status) }
        return try? JSONDecoder().decode(Credentials.self, from: data)
    }

    public func save(_ c: Credentials) throws {
        // Replace rather than update, so a changed attribute takes effect.
        try delete()
        let status = SecItemAdd(addQuery(data: try JSONEncoder().encode(c)) as CFDictionary, nil)
        guard status == errSecSuccess else { throw KeychainError(status) }
    }

    public func delete() throws {
        let status = SecItemDelete(baseQuery() as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else { throw KeychainError(status) }
    }

    /// Moves the credentials of the first builds (three device-only items,
    /// service `heraldo`) into the synced item, then deletes them. Does
    /// nothing once they are gone. Returns whether it moved anything.
    @discardableResult
    public func migrateLegacy() -> Bool {
        let legacy = Credentials(serverURL: readLegacy("server-url"), readToken: readLegacy("read-token"),
                                 writeToken: readLegacy("write-token"))
        guard legacy.serverURL != nil || legacy.readToken != nil || legacy.writeToken != nil else { return false }
        if (try? read()) == nil {
            guard (try? save(legacy)) != nil else { return false } // keep the old items for the next try
        }
        for account in ["server-url", "read-token", "write-token"] {
            for q in legacyQueries(account) { SecItemDelete(q as CFDictionary) }
        }
        return true
    }

    // MARK: Queries

    func baseQuery() -> [String: Any] {
        var q: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: Self.service,
            kSecAttrAccount as String: Self.account,
        ]
        switch mode {
        case .shared(let group):
            q[kSecUseDataProtectionKeychain as String] = true
            if !group.isEmpty { q[kSecAttrAccessGroup as String] = group }
            q[kSecAttrSynchronizable as String] = true
        #if os(macOS)
        case .local:
            q[kSecUseDataProtectionKeychain as String] = false
        #endif
        }
        return q
    }

    func addQuery(data: Data) -> [String: Any] {
        var q = baseQuery()
        q[kSecValueData as String] = data
        q[kSecAttrLabel as String] = "Heraldo"
        if case .shared = mode {
            // Readable by the extension while the phone is locked, and not a
            // ThisDeviceOnly class, which iCloud Keychain does not sync.
            q[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlock
        }
        return q
    }

    /// The first builds' items: one per field, device-only, in the app group
    /// or, for an unsigned build, in the app's own group.
    func legacyQueries(_ account: String) -> [[String: Any]] {
        let base: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: "heraldo",
            kSecAttrAccount as String: account,
            kSecUseDataProtectionKeychain as String: true,
        ]
        var out = [base]
        if case .shared(let group) = mode, !group.isEmpty {
            var q = base
            q[kSecAttrAccessGroup as String] = group
            out.insert(q, at: 0)
        }
        return out
    }

    private func readLegacy(_ account: String) -> String? {
        for var q in legacyQueries(account) {
            q[kSecReturnData as String] = true
            q[kSecMatchLimit as String] = kSecMatchLimitOne
            var out: CFTypeRef?
            if SecItemCopyMatching(q as CFDictionary, &out) == errSecSuccess, let data = out as? Data {
                return String(data: data, encoding: .utf8)
            }
        }
        return nil
    }
}

public struct KeychainError: LocalizedError, Equatable {
    public let status: OSStatus
    public init(_ status: OSStatus) { self.status = status }

    public var errorDescription: String? {
        let message = SecCopyErrorMessageString(status, nil) as String? ?? "Keychain error"
        if status == errSecMissingEntitlement {
            return "\(message) (\(status)). This build lacks the keychain entitlement."
        }
        return "\(message) (\(status))."
    }
}
