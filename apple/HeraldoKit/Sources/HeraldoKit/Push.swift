import Foundation

/// The `heraldo` object of a push payload (docs/api.md, "Payload").
public struct PushInfo: Equatable, Sendable {
    public var id: String
    public var kind: String?
    public var source: String?
    public var key: String?
    public var action: String?
    public var url: String?
    public var image: String?
    public var truncated: Bool

    public init?(userInfo: [AnyHashable: Any]) {
        guard let h = userInfo["heraldo"] as? [String: Any], let id = h["id"] as? String, !id.isEmpty else {
            return nil
        }
        self.id = id
        kind = h["kind"] as? String
        source = h["source"] as? String
        key = h["key"] as? String
        action = h["action"] as? String
        url = h["url"] as? String
        image = h["image"] as? String
        truncated = (h["truncated"] as? Bool) ?? ((h["truncated"] as? NSNumber)?.boolValue ?? false)
    }
}

/// The notification categories and actions, as the server sets `category`.
public enum PushCategory {
    /// An incident open, update or untyped: Ack and Open.
    public static let incident = "incident"
    /// Everything else: Open.
    public static let event = "event"

    public static let ackAction = "ack"
    public static let openAction = "open"
}

public enum APNs {
    /// The APNs environment of this build, from the aps-environment
    /// entitlement in its embedded provisioning profile. A TestFlight or App
    /// Store build has no embedded profile and is production; a simulator
    /// build is sandbox.
    public static func environment(profile: Data?, simulator: Bool) -> APNsEnvironment {
        if simulator { return .sandbox }
        guard let profile, let value = apsEnvironment(profile: profile) else { return .production }
        return value == "development" ? .sandbox : .production
    }

    /// The aps-environment entitlement of a provisioning profile. The profile
    /// is a CMS envelope around a property list; this reads the plist out of it.
    public static func apsEnvironment(profile: Data) -> String? {
        guard let start = profile.range(of: Data("<?xml".utf8)),
              let end = profile.range(of: Data("</plist>".utf8), in: start.lowerBound..<profile.endIndex) else {
            return nil
        }
        let plist = profile[start.lowerBound..<end.upperBound]
        guard let dict = try? PropertyListSerialization.propertyList(from: plist, format: nil) as? [String: Any],
              let ent = dict["Entitlements"] as? [String: Any] else { return nil }
        return ent["aps-environment"] as? String
    }

    /// The hex form of an APNs device token.
    public static func hex(_ token: Data) -> String {
        token.map { String(format: "%02x", $0) }.joined()
    }
}

public extension AppInfo {
    /// "0.1.0 (202610271330)": the version and build, for app_version.
    static func version(_ bundle: Bundle = .main) -> String {
        let v = bundle.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "?"
        let b = bundle.object(forInfoDictionaryKey: "CFBundleVersion") as? String ?? "?"
        return "\(v) (\(b))"
    }

    /// The source the app posts events and acks under.
    static let source = "heraldo-app"
}
