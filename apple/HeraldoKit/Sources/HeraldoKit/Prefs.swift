import Foundation

/// The app group a bundle names in its Info.plist (`HeraldoAppGroup`), or nil
/// when the build sets none.
public enum AppGroup {
    public static func identifier(_ bundle: Bundle = .main) -> String? {
        let raw = (bundle.object(forInfoDictionaryKey: CredentialStore.accessGroupInfoKey) as? String)?
            .trimmingCharacters(in: .whitespaces) ?? ""
        return raw.isEmpty || raw.hasPrefix("$(") ? nil : raw
    }

    /// The group's shared defaults, else this bundle's own.
    public static func defaults(_ bundle: Bundle = .main) -> UserDefaults {
        identifier(bundle).flatMap(UserDefaults.init(suiteName:)) ?? .standard
    }

    /// The group's shared folder, else nil.
    public static func container(_ bundle: Bundle = .main) -> URL? {
        identifier(bundle).flatMap { FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: $0) }
    }
}

/// How the notification service extension threads notifications.
public enum GroupBy: String, CaseIterable, Sendable {
    /// The server's thread-id: the event's thread, else its source. The default.
    case thread
    /// One thread per project, `project:<name>`; events with none keep the server's.
    case project

    /// The thread identifier for an event of `project`, or nil to keep the server's.
    public func threadIdentifier(project: String?) -> String? {
        guard self == .project, let p = project?.trimmingCharacters(in: .whitespaces), !p.isEmpty else { return nil }
        return "project:" + p
    }
}

/// The part of NSUbiquitousKeyValueStore that `SyncedPrefs` uses, so tests
/// can stand in for iCloud.
public protocol PrefsCloud: AnyObject {
    func object(forKey key: String) -> Any?
    func set(_ value: Any?, forKey key: String)
    func removeObject(forKey key: String)
    func synchronize() -> Bool
}

extension NSUbiquitousKeyValueStore: PrefsCloud {}

/// The app's display preferences, synced through the iCloud key-value store
/// (README, "iOS app", "Display").
///
/// Only display prefs live here. Push decisions are server settings, the
/// credentials are in iCloud Keychain, and device-only state (layout, Dock
/// policy, push token, permissions, caches) stays in plain defaults.
///
/// The local mirror is the app group's defaults, so the notification service
/// extension and the widgets read the same values without iCloud. Reads come
/// from the mirror; a write goes to both. Without the iCloud key-value
/// entitlement `synchronize()` fails, and the prefs stay on this device.
public final class SyncedPrefs: @unchecked Sendable {
    /// The iCloud keys. The mirror stores each under `prefs.<key>`.
    public enum Key: String, CaseIterable, Sendable {
        case groupBy = "notify.groupBy"
        case defaultView = "app.defaultView"
        case inboxKind = "inbox.kind"
        case inboxSource = "inbox.source"
        case inboxHold = "inbox.hold"
        case inboxProject = "inbox.project"
        case showRoutine = "inbox.showRoutine"
        case shareProject = "share.project"

        /// The mirror's key. The share project keeps the share extensions'
        /// own key (`SharePrefs.lastProject`), so their last choice and the
        /// synced pref are one value.
        var mirror: String {
            self == .shareProject ? SharePrefs.lastProjectKey : "prefs." + rawValue
        }

        /// Keys an extension writes to the mirror alone; `pushMirror()`
        /// carries them to iCloud.
        static let writtenByExtensions: [Key] = [.shareProject]
    }

    /// Set in the mirror once this device has copied its values to iCloud.
    static let seededKey = "prefs.seeded"

    public let local: UserDefaults
    /// Nil when iCloud is not available to this build: prefs stay local.
    public let cloud: PrefsCloud?

    /// `cloud` is used only when its `synchronize()` succeeds, which needs
    /// the entitlement. Values already in iCloud replace the mirror's; on a
    /// device's first run with iCloud, the mirror's values seed the keys
    /// iCloud lacks.
    public init(local: UserDefaults, cloud: PrefsCloud?) {
        self.local = local
        self.cloud = cloud.flatMap { $0.synchronize() ? $0 : nil }
        guard let cloud = self.cloud else { return }
        let seeded = local.bool(forKey: Self.seededKey)
        for key in Key.allCases {
            if let v = cloud.object(forKey: key.rawValue) {
                local.set(v, forKey: key.mirror)
            } else if !seeded, let v = local.object(forKey: key.mirror) {
                cloud.set(v, forKey: key.rawValue)
            }
        }
        local.set(true, forKey: Self.seededKey)
    }

    /// The app's store: the app group's mirror and the default iCloud store.
    public static func live(_ bundle: Bundle = .main) -> SyncedPrefs {
        SyncedPrefs(local: AppGroup.defaults(bundle), cloud: NSUbiquitousKeyValueStore.default)
    }

    /// The mirror alone, for an extension: no iCloud calls.
    public static func mirror(_ bundle: Bundle = .main) -> SyncedPrefs {
        SyncedPrefs(local: AppGroup.defaults(bundle), cloud: nil)
    }

    /// Whether the prefs sync through iCloud.
    public var syncs: Bool { cloud != nil }

    public var modeDescription: String { syncs ? "iCloud" : "this device only" }

    // MARK: Values

    public var groupBy: GroupBy {
        get { string(.groupBy).flatMap(GroupBy.init(rawValue:)) ?? .thread }
        set { set(newValue == .thread ? nil : newValue.rawValue, .groupBy) }
    }

    /// The view the app opens on: a home (`inbox`, `incidents`, `checkins`)
    /// or a saved view, `view:<id>`. Nil opens the platform's default.
    public var defaultView: String? {
        get { string(.defaultView) }
        set { set(newValue, .defaultView) }
    }

    /// The Inbox's filter at launch.
    public var inboxFilter: InboxFilter {
        get {
            InboxFilter(kind: string(.inboxKind), source: string(.inboxSource), hold: string(.inboxHold),
                        project: string(.inboxProject), showRoutine: local.bool(forKey: Key.showRoutine.mirror))
        }
        set {
            set(newValue.kind, .inboxKind)
            set(newValue.source, .inboxSource)
            set(newValue.hold, .inboxHold)
            set(newValue.project, .inboxProject)
            set(newValue.showRoutine ? true : nil, .showRoutine)
        }
    }

    /// The project the share extension and Send to Heraldo offer first.
    public var shareProject: String? {
        get { string(.shareProject) }
        set { set(newValue, .shareProject) }
    }

    // MARK: Changes from iCloud

    /// Copies the changed keys from iCloud into the mirror and returns the
    /// keys it copied. `userInfo` is a didChangeExternallyNotification's;
    /// without its key list every key is copied.
    @discardableResult
    public func pull(_ userInfo: [AnyHashable: Any]?) -> [Key] {
        guard let cloud else { return [] }
        let names = userInfo?[NSUbiquitousKeyValueStoreChangedKeysKey] as? [String]
        let keys = names.map { $0.compactMap(Key.init(rawValue:)) } ?? Key.allCases
        for key in keys {
            if let v = cloud.object(forKey: key.rawValue) {
                local.set(v, forKey: key.mirror)
            } else {
                local.removeObject(forKey: key.mirror)
            }
        }
        return keys
    }

    /// Calls `onChange` on the main queue after iCloud changes a pref.
    /// Returns nil when the prefs do not sync. Keep the token while observing.
    public func observe(_ onChange: @escaping @Sendable ([Key]) -> Void) -> NSObjectProtocol? {
        guard let store = cloud as? NSUbiquitousKeyValueStore else { return nil }
        return NotificationCenter.default.addObserver(
            forName: NSUbiquitousKeyValueStore.didChangeExternallyNotification, object: store, queue: .main
        ) { [weak self] note in
            guard let self else { return }
            let keys = self.pull(note.userInfo)
            if !keys.isEmpty { onChange(keys) }
        }
    }

    /// Copies to iCloud what an extension changed in the mirror since, such
    /// as the share sheet's last project. The app calls it at launch and on
    /// each sync. Returns whether it copied anything.
    @discardableResult
    public func pushMirror() -> Bool {
        guard let cloud else { return false }
        var changed = false
        for key in Key.writtenByExtensions {
            let mine = local.object(forKey: key.mirror)
            if !Self.same(mine, cloud.object(forKey: key.rawValue)) {
                if let mine { cloud.set(mine, forKey: key.rawValue) } else { cloud.removeObject(forKey: key.rawValue) }
                changed = true
            }
        }
        return changed
    }

    private static func same(_ a: Any?, _ b: Any?) -> Bool {
        switch (a, b) {
        case (nil, nil): return true
        case let (a as NSObject, b as NSObject): return a.isEqual(b)
        default: return false
        }
    }

    // MARK: Storage

    private func string(_ key: Key) -> String? {
        local.string(forKey: key.mirror).flatMap { $0.isEmpty ? nil : $0 }
    }

    private func set(_ value: Any?, _ key: Key) {
        if let s = value as? String, s.isEmpty { return set(nil, key) }
        if let value {
            local.set(value, forKey: key.mirror)
            cloud?.set(value, forKey: key.rawValue)
        } else {
            local.removeObject(forKey: key.mirror)
            cloud?.removeObject(forKey: key.rawValue)
        }
    }
}

/// The Inbox filters the app restores at launch.
public struct InboxFilter: Equatable, Sendable {
    public var kind: String?
    public var source: String?
    public var hold: String?
    public var project: String?
    public var showRoutine: Bool

    public init(kind: String? = nil, source: String? = nil, hold: String? = nil, project: String? = nil,
                showRoutine: Bool = false) {
        self.kind = kind
        self.source = source
        self.hold = hold
        self.project = project
        self.showRoutine = showRoutine
    }
}
