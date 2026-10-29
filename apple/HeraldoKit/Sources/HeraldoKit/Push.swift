import Foundation

/// The `heraldo` object of a push payload (docs/api.md, "Payload").
public struct PushInfo: Equatable, Sendable {
    public var id: String
    public var kind: String?
    public var source: String?
    public var key: String?
    public var action: String?
    /// The event's project, for grouping by project.
    public var project: String?
    public var url: String?
    public var image: String?
    /// The registry icon's name, for `GET /v1/icons/{name}`.
    public var icon: String?
    /// An https icon the event links to.
    public var iconURL: String?
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
        project = (h["project"] as? String).flatMap { $0.isEmpty ? nil : $0 }
        url = h["url"] as? String
        image = h["image"] as? String
        icon = h["icon"] as? String
        iconURL = h["icon_url"] as? String
        truncated = (h["truncated"] as? Bool) ?? ((h["truncated"] as? NSNumber)?.boolValue ?? false)
    }
}

/// The notification categories and actions, as the server sets `category`.
public enum PushCategory {
    /// An incident open, update or untyped: Ack, Snooze 1 h, Mute 1 h and Open.
    public static let incident = "incident"
    /// Everything else: Mute 1 h and Open.
    public static let event = "event"

    public static let ackAction = "ack"
    public static let muteAction = "mute"
    public static let snoozeAction = "snooze"
    public static let openAction = "open"

    /// How long Mute lasts, as the server reads it.
    public static let muteFor = "1h"
    /// How long Snooze lasts.
    public static let snoozeFor = "1h"

    /// What Snooze 1 h asks for: a mute of the incident's key that pushes
    /// the incident again when it ends, if it is still open. nil for an
    /// event without an incident key.
    public static func snoozeInput(_ info: PushInfo) -> MuteInput? {
        guard info.kind == "incident", let key = info.key, !key.isEmpty else { return nil }
        return MuteInput(key: key, for: snoozeFor, remind: true)
    }

    /// What Mute 1 h holds back: an incident's key; for a check-in, its
    /// check's incident key `checkin:<key>`, which the server applies to the
    /// check's own runs too; else the event's source.
    public static func muteInput(_ info: PushInfo) -> MuteInput? {
        if info.kind == "incident", let key = info.key, !key.isEmpty {
            return MuteInput(source: nil, key: key, for: muteFor)
        }
        if info.kind == "checkin", let key = info.key, !key.isEmpty {
            return MuteInput(source: nil, key: "checkin:" + key, for: muteFor)
        }
        guard let source = info.source, !source.isEmpty else { return nil }
        return MuteInput(source: source, for: muteFor)
    }
}

public enum APNs {
    /// The APNs environment of this build, from the aps-environment
    /// entitlement in its embedded provisioning profile. A TestFlight or App
    /// Store build has no embedded profile and is production; a simulator
    /// build is sandbox. A Mac app embeds its profile too.
    public static func environment(profile: Data?, simulator: Bool) -> APNsEnvironment {
        if simulator { return .sandbox }
        guard let profile, let value = apsEnvironment(profile: profile) else { return .production }
        return value == "development" ? .sandbox : .production
    }

    /// The aps-environment entitlement of a provisioning profile: an iOS
    /// `.mobileprovision` or a macOS `.provisionprofile`, whose key carries the
    /// `com.apple.developer.` prefix. The profile is a CMS envelope around a
    /// property list; this reads the plist out of it.
    public static func apsEnvironment(profile: Data) -> String? {
        guard let start = profile.range(of: Data("<?xml".utf8)),
              let end = profile.range(of: Data("</plist>".utf8), in: start.lowerBound..<profile.endIndex) else {
            return nil
        }
        let plist = profile[start.lowerBound..<end.upperBound]
        guard let dict = try? PropertyListSerialization.propertyList(from: plist, format: nil) as? [String: Any],
              let ent = dict["Entitlements"] as? [String: Any] else { return nil }
        return (ent["aps-environment"] ?? ent["com.apple.developer.aps-environment"]) as? String
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
