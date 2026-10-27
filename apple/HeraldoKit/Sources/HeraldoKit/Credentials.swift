import Foundation
import Security

/// The server URL and the token pair the person pastes into Settings.
public struct Credentials: Equatable, Sendable {
    public var serverURL: String?
    public var readToken: String?
    public var writeToken: String?

    public init(serverURL: String? = nil, readToken: String? = nil, writeToken: String? = nil) {
        self.serverURL = serverURL
        self.readToken = readToken
        self.writeToken = writeToken
    }

    public var isComplete: Bool {
        !(serverURL ?? "").isEmpty && !(readToken ?? "").isEmpty
    }
}

/// Keeps the credentials in the Keychain, shared with the notification
/// service extension through the app group. The tokens and the URL are three
/// generic-password items under one service.
public struct CredentialStore: Sendable {
    public static let service = "heraldo"

    /// The app group, from the bundle's `HeraldoAppGroup` Info.plist key.
    public var accessGroup: String?

    public init(accessGroup: String? = CredentialStore.appGroup()) {
        self.accessGroup = accessGroup
    }

    public static func appGroup(_ bundle: Bundle = .main) -> String? {
        guard let g = bundle.object(forInfoDictionaryKey: "HeraldoAppGroup") as? String,
              !g.isEmpty, !g.contains("$(") else { return nil }
        return g
    }

    public func load() -> Credentials {
        Credentials(serverURL: read("server-url"), readToken: read("read-token"), writeToken: read("write-token"))
    }

    public func save(_ c: Credentials) throws {
        try write("server-url", c.serverURL)
        try write("read-token", c.readToken)
        try write("write-token", c.writeToken)
    }

    // MARK: Keychain

    private func query(_ account: String, group: String?) -> [String: Any] {
        var q: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: Self.service,
            kSecAttrAccount as String: account,
            kSecUseDataProtectionKeychain as String: true,
        ]
        if let group { q[kSecAttrAccessGroup as String] = group }
        return q
    }

    /// The access groups to try in order. An unsigned simulator build has no
    /// app group entitlement, so the Keychain refuses the group; it falls back
    /// to the app's own items.
    private var groups: [String?] { accessGroup == nil ? [nil] : [accessGroup, nil] }

    private func read(_ account: String) -> String? {
        for group in groups {
            var q = query(account, group: group)
            q[kSecReturnData as String] = true
            q[kSecMatchLimit as String] = kSecMatchLimitOne
            var out: CFTypeRef?
            let status = SecItemCopyMatching(q as CFDictionary, &out)
            if status == errSecSuccess, let data = out as? Data {
                return String(data: data, encoding: .utf8)
            }
            if status != errSecMissingEntitlement && status != errSecItemNotFound { return nil }
        }
        return nil
    }

    private func write(_ account: String, _ value: String?) throws {
        var last: OSStatus = errSecSuccess
        for group in groups {
            let q = query(account, group: group)
            let deleted = SecItemDelete(q as CFDictionary)
            if deleted == errSecMissingEntitlement { last = deleted; continue }
            guard let value, !value.isEmpty else { return }
            var add = q
            add[kSecValueData as String] = Data(value.utf8)
            // The extension reads the token while the phone is locked.
            add[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlock
            last = SecItemAdd(add as CFDictionary, nil)
            if last == errSecSuccess { return }
            if last != errSecMissingEntitlement { break }
        }
        if last != errSecSuccess {
            throw HeraldoError.badAnswer("Keychain error \(last)")
        }
    }
}
