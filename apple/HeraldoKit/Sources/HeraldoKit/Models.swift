import Foundation

/// A stored event as the read API returns it (`$defs/Event` in the schema).
/// Only `id` and `source` are certain; every other field is optional so an
/// older or newer server still decodes.
public struct Event: Codable, Hashable, Identifiable, Sendable {
    public var id: String
    public var receivedAt: Date?
    public var ts: Date?
    public var source: String
    public var kind: String?
    public var key: String?
    public var action: String?
    public var host: String?
    public var title: String?
    public var summary: String?
    public var body: String?
    public var level: String?
    public var thread: String?
    public var url: String?
    public var image: String?
    public var icon: String?
    public var tags: [String]?
    public var actions: [EventAction]?
    public var hold: Hold?
    public var delivery: [Delivery]?
    public var checkin: Checkin?

    enum CodingKeys: String, CodingKey {
        case id, ts, source, kind, key, action, host, title, summary, body, level, thread
        case url, image, icon, tags, actions, hold, delivery, checkin
        case receivedAt = "received_at"
    }

    public init(id: String, source: String, kind: String? = nil, title: String? = nil,
                summary: String? = nil, body: String? = nil, receivedAt: Date? = nil) {
        self.id = id
        self.source = source
        self.kind = kind
        self.title = title
        self.summary = summary
        self.body = body
        self.receivedAt = receivedAt
    }

    /// The line a list shows: the title, else the summary, else the source.
    public var headline: String {
        for s in [title, summary] {
            if let s, !s.isEmpty { return s }
        }
        return source
    }

    /// The time to show: when the event happened, else when it arrived.
    public var date: Date? { ts ?? receivedAt }

    /// An incident event that is open, updated or untyped: Ack applies.
    public var isOpenIncident: Bool {
        kind == "incident" && (action == nil || action == "open" || action == "update")
    }

    /// A check-in whose last run failed: `fail`, or a non-zero exit code on
    /// anything but `log` (the push table's rule, docs/api.md).
    public var isFailingCheckin: Bool {
        guard kind == "checkin" else { return false }
        if action == "fail" { return true }
        if action == "log" { return false }
        return (checkin?.exitCode ?? 0) != 0
    }
}

/// Run details of a check-in (docs/api.md, "Checkin").
public struct Checkin: Codable, Hashable, Sendable {
    public var rid: String?
    public var exitCode: Int?
    public var durationMs: Int?
    public var schedule: String?

    enum CodingKeys: String, CodingKey {
        case rid, schedule
        case exitCode = "exit_code"
        case durationMs = "duration_ms"
    }

    public init(rid: String? = nil, exitCode: Int? = nil) {
        self.rid = rid
        self.exitCode = exitCode
    }
}

public struct EventAction: Codable, Hashable, Sendable {
    public var id: String
    public var title: String
    public var type: String
    public var url: String?
}

public struct Hold: Codable, Hashable, Sendable {
    public var state: String?
    public var reason: String?
}

public struct Delivery: Codable, Hashable, Sendable {
    public var device: String
    public var outcome: String
    public var level: String?
    public var at: Date?
    public var error: String?
}

/// One page of `GET /v1/events`.
public struct EventPage: Codable, Sendable {
    public var events: [Event]
    public var nextCursor: String?

    enum CodingKeys: String, CodingKey {
        case events
        case nextCursor = "next_cursor"
    }
}

/// `GET /v1/state`: the latest event per key. Only incidents are used yet.
public struct ServerState: Codable, Sendable {
    public var incidents: [Event]
    public var progress: [Event]?
    public var checkins: [Event]?
    public var statuses: [Event]?
}

/// A read cursor, such as `inbox`.
public struct ReadCursor: Codable, Hashable, Sendable {
    public var name: String?
    public var cursor: String
    /// The events after the cursor that push an alert: the badge.
    public var unread: Int?
}

/// What the app reports about its notification permissions. Nil is unknown.
public struct Permissions: Codable, Hashable, Sendable {
    public var alerts: Bool?
    public var timeSensitive: Bool?
    public var liveActivities: Bool?

    enum CodingKeys: String, CodingKey {
        case alerts
        case timeSensitive = "time_sensitive"
        case liveActivities = "live_activities"
    }

    public init(alerts: Bool? = nil, timeSensitive: Bool? = nil, liveActivities: Bool? = nil) {
        self.alerts = alerts
        self.timeSensitive = timeSensitive
        self.liveActivities = liveActivities
    }
}

public enum APNsEnvironment: String, Codable, Sendable {
    case sandbox, production
}

/// The body of `POST /v1/devices` and `PUT /v1/devices/{id}`.
public struct DeviceRegistration: Codable, Hashable, Sendable {
    public var token: String
    public var environment: APNsEnvironment
    public var topic: String
    public var name: String?
    public var platform: String?
    public var appVersion: String?
    public var permissions: Permissions?

    enum CodingKeys: String, CodingKey {
        case token, environment, topic, name, platform, permissions
        case appVersion = "app_version"
    }

    public init(token: String, environment: APNsEnvironment, topic: String, name: String?,
                platform: String?, appVersion: String?, permissions: Permissions?) {
        self.token = token
        self.environment = environment
        self.topic = topic
        self.name = name
        self.platform = platform
        self.appVersion = appVersion
        self.permissions = permissions
    }
}

public struct Device: Codable, Hashable, Sendable {
    public var id: String
    public var environment: APNsEnvironment
    public var topic: String
    public var tokenSuffix: String
    public var name: String?
    public var active: Bool
    public var permissions: Permissions?

    enum CodingKeys: String, CodingKey {
        case id, environment, topic, name, active, permissions
        case tokenSuffix = "token_suffix"
    }
}

public struct Health: Codable, Sendable {
    public var status: String
    public var version: String
    public var db: String
    public var events: Int?
    public var push: String?
}

/// What a sender posts: the subset of `EventInput` the app sends.
public struct EventInput: Codable, Hashable, Sendable {
    public var source: String
    public var kind: String?
    public var key: String?
    public var action: String?
    public var host: String?
    public var title: String?
    public var body: String?
    public var level: String?

    public init(source: String, kind: String? = nil, key: String? = nil, action: String? = nil,
                host: String? = nil, title: String? = nil, body: String? = nil, level: String? = nil) {
        self.source = source
        self.kind = kind
        self.key = key
        self.action = action
        self.host = host
        self.title = title
        self.body = body
        self.level = level
    }
}

/// The envelope's interruption levels.
public enum Level: String, CaseIterable, Codable, Sendable {
    case passive, active
    case timeSensitive = "time-sensitive"
    case critical
}

/// The envelope's kinds.
public enum Kind: String, CaseIterable, Codable, Sendable {
    case message, digest, incident, progress, checkin, status
}
