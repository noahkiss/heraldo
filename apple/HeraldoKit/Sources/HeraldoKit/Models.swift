import Foundation

/// A stored event as the read API returns it (`$defs/Event` in the schema).
/// Only `id` and `source` are certain; every other field is optional so an
/// older or newer server still decodes.
public struct Event: Codable, Hashable, Identifiable, Sendable {
    public var id: String
    public var receivedAt: Date?
    /// The event's list cursor. Servers before 0.3.1 send none.
    public var cursor: String?
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
    /// The project the event is filed under. Servers before 0.5.0 send none.
    public var project: String?
    public var url: String?
    public var image: String?
    public var icon: String?
    public var tags: [String]?
    public var actions: [EventAction]?
    public var hold: Hold?
    public var delivery: [Delivery]?
    /// One line on the push decision, such as "pushed active to 2 devices".
    /// Servers before 0.7 send none.
    public var why: String?
    /// Ids of the files stored with the event. Servers before 0.7 send none.
    public var attachments: [String]?
    /// Each attachment's id, type and size, in the order of `attachments`.
    public var attachmentInfo: [AttachmentInfo]?
    public var checkin: Checkin?
    /// On an incident event: the incident as it stood after the event.
    public var incident: IncidentInfo?

    enum CodingKeys: String, CodingKey {
        case id, cursor, ts, source, kind, key, action, host, title, summary, body, level, thread, project
        case url, image, icon, tags, actions, hold, delivery, why, checkin, incident, attachments
        case receivedAt = "received_at"
        case attachmentInfo = "attachment_info"
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

    /// The latest event of an incident someone acknowledged: the server's
    /// incident state, else the event's own action (servers before stage 5).
    public var isAcked: Bool {
        if let state = incident?.state { return state == "acked" }
        return action == "ack"
    }

    /// A held-back push: a mute or an inhibit rule skipped it on every device.
    public var wasHeldBack: Bool {
        guard let d = delivery, !d.isEmpty else { return false }
        return d.allSatisfy { $0.outcome == "skipped" }
    }

    /// A check-in whose last run failed: `fail`, or a non-zero exit code on
    /// anything but `log` (the push table's rule, docs/api.md).
    public var isFailingCheckin: Bool {
        guard kind == "checkin" else { return false }
        if action == "fail" { return true }
        if action == "log" { return false }
        return (checkin?.exitCode ?? 0) != 0
    }

    /// A routine event: one the push table never pushes by kind and action.
    /// Check-ins that did not fail, progress and status (docs/api.md,
    /// "Routine events"). The Inbox leaves them out by default.
    public var isRoutine: Bool {
        switch kind {
        case "progress", "status": return true
        case "checkin": return !isFailingCheckin
        default: return false
        }
    }

    /// A kind whose events are routine unless a check-in failed.
    public static func isRoutineKind(_ kind: String?) -> Bool {
        kind == "checkin" || kind == "progress" || kind == "status"
    }
}

/// Run details of a check-in (docs/api.md, "Checkin").
public struct Checkin: Codable, Hashable, Sendable {
    public var rid: String?
    public var exitCode: Int?
    public var durationMs: Int?
    public var schedule: CheckSchedule?

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

/// When a check expects pings (docs/api.md, "Check-ins"). A sender's
/// `checkin.schedule` has the same fields, less `reminder_s`.
public struct CheckSchedule: Codable, Hashable, Sendable {
    public var periodS: Int?
    public var cron: String?
    public var tz: String?
    public var graceS: Int?
    public var maxRuntimeS: Int?
    public var reminderS: Int?

    enum CodingKeys: String, CodingKey {
        case cron, tz
        case periodS = "period_s"
        case graceS = "grace_s"
        case maxRuntimeS = "max_runtime_s"
        case reminderS = "reminder_s"
    }

    public init(periodS: Int? = nil, cron: String? = nil, tz: String? = nil, graceS: Int? = nil,
                maxRuntimeS: Int? = nil, reminderS: Int? = nil) {
        self.periodS = periodS
        self.cron = cron
        self.tz = tz
        self.graceS = graceS
        self.maxRuntimeS = maxRuntimeS
        self.reminderS = reminderS
    }

    /// One line, such as "every 1h, grace 10m" or "cron 0 3 * * * Europe/Madrid".
    public var summary: String {
        var parts: [String] = []
        if let periodS {
            parts.append("every \(Self.duration(periodS))")
        } else if let cron {
            parts.append(["cron", cron, tz].compactMap { $0 }.joined(separator: " "))
        } else {
            parts.append("unscheduled")
        }
        if let graceS { parts.append("grace \(Self.duration(graceS))") }
        if let maxRuntimeS { parts.append("max runtime \(Self.duration(maxRuntimeS))") }
        return parts.joined(separator: ", ")
    }

    /// Seconds as the largest units that fit, such as 1h30m or 45s.
    public static func duration(_ seconds: Int) -> String {
        if seconds == 0 { return "0s" }
        var n = seconds
        var out = ""
        for (unit, size) in [("d", 86400), ("h", 3600), ("m", 60), ("s", 1)] where n >= size {
            out += "\(n / size)\(unit)"
            n %= size
        }
        return out
    }
}

/// A check from `GET /v1/checkins`: a job that pings on a schedule.
public struct Check: Codable, Hashable, Identifiable, Sendable {
    public enum State: String, Codable, Sendable {
        case new, up, started, late, down, paused
    }

    public var key: String
    public var host: String?
    /// The project the check is filed under, on servers that set one.
    public var project: String?
    public var state: State
    public var stateSince: Date?
    public var downReason: String?
    public var schedule: CheckSchedule?
    public var createdAt: Date?
    public var lastPing: Date?
    public var lastEvent: String?
    public var startedAt: Date?
    public var lastSuccess: Date?
    public var lastDurationMs: Int?
    public var nextDue: Date?
    public var downAfter: Date?
    public var lastLog: String?
    public var lastLogAt: Date?
    public var incident: String?

    public var id: String { key }

    enum CodingKeys: String, CodingKey {
        case key, host, project, state, schedule, incident
        case stateSince = "state_since"
        case downReason = "down_reason"
        case createdAt = "created_at"
        case lastPing = "last_ping"
        case lastEvent = "last_event"
        case startedAt = "started_at"
        case lastSuccess = "last_success"
        case lastDurationMs = "last_duration_ms"
        case nextDue = "next_due"
        case downAfter = "down_after"
        case lastLog = "last_log"
        case lastLogAt = "last_log_at"
    }

    public init(key: String, state: State) {
        self.key = key
        self.state = state
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        key = try c.decode(String.self, forKey: .key)
        // A state this build does not know reads as new, so a newer server
        // still decodes.
        state = (try? c.decode(State.self, forKey: .state)) ?? .new
        host = try c.decodeIfPresent(String.self, forKey: .host)
        project = try c.decodeIfPresent(String.self, forKey: .project)
        stateSince = try c.decodeIfPresent(Date.self, forKey: .stateSince)
        downReason = try c.decodeIfPresent(String.self, forKey: .downReason)
        schedule = try c.decodeIfPresent(CheckSchedule.self, forKey: .schedule)
        createdAt = try c.decodeIfPresent(Date.self, forKey: .createdAt)
        lastPing = try c.decodeIfPresent(Date.self, forKey: .lastPing)
        lastEvent = try c.decodeIfPresent(String.self, forKey: .lastEvent)
        startedAt = try c.decodeIfPresent(Date.self, forKey: .startedAt)
        lastSuccess = try c.decodeIfPresent(Date.self, forKey: .lastSuccess)
        lastDurationMs = try c.decodeIfPresent(Int.self, forKey: .lastDurationMs)
        nextDue = try c.decodeIfPresent(Date.self, forKey: .nextDue)
        downAfter = try c.decodeIfPresent(Date.self, forKey: .downAfter)
        lastLog = try c.decodeIfPresent(String.self, forKey: .lastLog)
        lastLogAt = try c.decodeIfPresent(Date.self, forKey: .lastLogAt)
        incident = try c.decodeIfPresent(String.self, forKey: .incident)
    }

    /// The last run's length, such as "1m42s", or nil when none was measured.
    public var lastDuration: String? {
        guard let ms = lastDurationMs else { return nil }
        if ms < 1000 { return "\(ms)ms" }
        return CheckSchedule.duration((ms + 500) / 1000)
    }

    /// The first line of the last log.
    public var lastLogLine: String? {
        guard let log = lastLog?.trimmingCharacters(in: .whitespacesAndNewlines), !log.isEmpty else { return nil }
        return String(log.split(separator: "\n", maxSplits: 1).first ?? "")
    }

    /// The state as a label, with the reason for down.
    public var stateLabel: String {
        switch state {
        case .new: return "New"
        case .up: return "Up"
        case .started: return "Running"
        case .late: return "Late"
        case .paused: return "Paused"
        case .down:
            switch downReason {
            case "failed": return "Failed"
            case "overran": return "Too long"
            default: return "Down"
            }
        }
    }
}

/// `GET /v1/checkins`.
public struct CheckList: Codable, Sendable {
    public var checkins: [Check]
}

public struct EventAction: Codable, Hashable, Sendable {
    public var id: String
    public var title: String
    public var type: String
    public var url: String?
}

/// Triage state from a bridge such as apprise-tee: `state` (such as held),
/// and the verdict or reason the triage gave.
public struct Hold: Codable, Hashable, Sendable {
    public var state: String?
    public var reason: String?
    public var verdict: String?

    public init(state: String? = nil, reason: String? = nil, verdict: String? = nil) {
        self.state = state
        self.reason = reason
        self.verdict = verdict
    }

    /// One line for a list row or the detail view: "held · flapping".
    public var label: String {
        [state, verdict, reason].compactMap { $0 }.filter { !$0.isEmpty }.joined(separator: " · ")
    }
}

/// The incident server field: the incident after an event.
public struct IncidentInfo: Codable, Hashable, Sendable {
    /// open, acked or resolved.
    public var state: String
    public var openedAt: Date?
    public var resolvedAt: Date?

    enum CodingKeys: String, CodingKey {
        case state
        case openedAt = "opened_at"
        case resolvedAt = "resolved_at"
    }

    public init(state: String, openedAt: Date? = nil, resolvedAt: Date? = nil) {
        self.state = state
        self.openedAt = openedAt
        self.resolvedAt = resolvedAt
    }

    /// How long the incident was open, for a resolved one.
    public var openFor: TimeInterval? {
        guard let openedAt, let resolvedAt, resolvedAt > openedAt else { return nil }
        return resolvedAt.timeIntervalSince(openedAt)
    }
}

/// A mute: pushes of a source, a key, a project, or a mix, are held back
/// until `until`.
public struct Mute: Codable, Hashable, Identifiable, Sendable {
    public var id: String
    public var source: String?
    public var key: String?
    public var project: String?
    public var until: Date
    public var createdAt: Date?
    public var endedAt: Date?
    public var active: Bool

    enum CodingKeys: String, CodingKey {
        case id, source, key, project, until, active
        case createdAt = "created_at"
        case endedAt = "ended_at"
    }
}

/// The body of `POST /v1/mutes`.
public struct MuteInput: Codable, Hashable, Sendable {
    public var source: String?
    public var key: String?
    /// Every event filed under this project. A server before project mutes
    /// refuses the field with 400.
    public var project: String?
    /// A Go duration such as 1h.
    public var `for`: String?

    public init(source: String? = nil, key: String? = nil, project: String? = nil, for duration: String) {
        self.source = source
        self.key = key
        self.project = project
        self.for = duration
    }
}

/// One project from `GET /v1/projects`.
public struct ProjectInfo: Codable, Hashable, Identifiable, Sendable {
    public var name: String
    public var events: Int
    public var lastAt: Date?

    public var id: String { name }

    enum CodingKeys: String, CodingKey {
        case name, events
        case lastAt = "last_at"
    }

    public init(name: String, events: Int, lastAt: Date? = nil) {
        self.name = name
        self.events = events
        self.lastAt = lastAt
    }
}

/// `GET /v1/projects`: the projects events name, and the count with none.
public struct ProjectList: Codable, Sendable {
    public var projects: [ProjectInfo]
    public var unfiled: Int
}

/// The project filter value for events with no project.
public let noProject = "-"

struct MuteList: Codable, Sendable {
    var mutes: [Mute]
}

/// One file stored with an event, without its bytes. `HeraldoClient.attachment(id:)`
/// fetches the bytes.
public struct AttachmentInfo: Codable, Hashable, Sendable, Identifiable {
    public var id: String
    public var contentType: String
    public var bytes: Int

    enum CodingKeys: String, CodingKey {
        case id, bytes
        case contentType = "content_type"
    }

    public init(id: String, contentType: String, bytes: Int) {
        self.id = id
        self.contentType = contentType
        self.bytes = bytes
    }

    /// An image the app can show inline.
    public var isImage: Bool { contentType.hasPrefix("image/") }
    /// Text the app can show inline: plain text or JSON.
    public var isText: Bool { contentType == "text/plain" || contentType == "application/json" }
}

public struct Delivery: Codable, Hashable, Sendable {
    public var device: String
    public var outcome: String
    public var level: String?
    public var at: Date?
    public var error: String?
}

/// Why an event did or did not push: `GET /v1/events/{id}/why`.
public struct PushTrace: Codable, Hashable, Sendable {
    public var eventId: String
    public var why: String?
    public var decisions: [PushDecision]

    enum CodingKeys: String, CodingKey {
        case why, decisions
        case eventId = "event_id"
    }
}

/// One push pipeline stage's decision: pass, downgrade, delay, merge or drop.
public struct PushDecision: Codable, Hashable, Sendable {
    public var stage: String
    public var outcome: String
    public var reason: String
    public var at: Date?
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

extension ServerState {
    /// The incidents that are not resolved: open before acked, newest first
    /// in each.
    public var sortedIncidents: [Event] {
        let newest: (Event, Event) -> Bool = { ($0.date ?? .distantPast) > ($1.date ?? .distantPast) }
        return incidents.sorted { a, b in a.isAcked == b.isAcked ? newest(a, b) : !a.isAcked }
    }

    /// One line per incident, for Shortcuts: state, headline, source and
    /// host, and when it opened (ISO 8601, UTC).
    public static func incidentsText(_ incidents: [Event]) -> String {
        if incidents.isEmpty { return "No open incidents" }
        let iso = ISO8601DateFormatter()
        return incidents.map { e in
            var line = "\(e.isAcked ? "acked" : "open"): \(e.headline) (\([e.source, e.host].compactMap { $0 }.joined(separator: ", ")))"
            if let opened = e.incident?.openedAt ?? e.date { line += ", since \(iso.string(from: opened))" }
            return line
        }.joined(separator: "\n")
    }
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
    /// The push-to-start token for Live Activities, hex.
    public var liveActivityToken: String?

    enum CodingKeys: String, CodingKey {
        case token, environment, topic, name, platform, permissions
        case appVersion = "app_version"
        case liveActivityToken = "live_activity_token"
    }

    public init(token: String, environment: APNsEnvironment, topic: String, name: String?,
                platform: String?, appVersion: String?, permissions: Permissions?,
                liveActivityToken: String? = nil) {
        self.token = token
        self.environment = environment
        self.topic = topic
        self.name = name
        self.platform = platform
        self.appVersion = appVersion
        self.permissions = permissions
        self.liveActivityToken = liveActivityToken
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
    /// Live Activity content, for kind progress.
    public var content: LivePatch?

    public init(source: String, kind: String? = nil, key: String? = nil, action: String? = nil,
                host: String? = nil, title: String? = nil, body: String? = nil, level: String? = nil,
                content: LivePatch? = nil) {
        self.source = source
        self.kind = kind
        self.key = key
        self.action = action
        self.host = host
        self.title = title
        self.body = body
        self.level = level
        self.content = content
    }

    // Encode only: LivePatch has no decoder.
    enum CodingKeys: String, CodingKey { case source, kind, key, action, host, title, body, level, content }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.init(source: try c.decode(String.self, forKey: .source),
                  kind: try c.decodeIfPresent(String.self, forKey: .kind),
                  key: try c.decodeIfPresent(String.self, forKey: .key),
                  action: try c.decodeIfPresent(String.self, forKey: .action),
                  host: try c.decodeIfPresent(String.self, forKey: .host),
                  title: try c.decodeIfPresent(String.self, forKey: .title),
                  body: try c.decodeIfPresent(String.self, forKey: .body),
                  level: try c.decodeIfPresent(String.self, forKey: .level))
    }
}

/// `GET /v1/activities/{key}`: the activity on a progress key.
public struct LiveActivityInfo: Codable, Hashable, Sendable {
    public var key: String
    public var id: String
    /// active or ended.
    public var state: String
    public var source: String
    public var startedAt: Date?
    public var updatedAt: Date?
    public var endedAt: Date?
    public var endReason: String?
    public var instances: [LiveInstance]

    enum CodingKeys: String, CodingKey {
        case key, id, state, source, instances
        case startedAt = "started_at"
        case updatedAt = "updated_at"
        case endedAt = "ended_at"
        case endReason = "end_reason"
    }
}

/// The activity on one device.
public struct LiveInstance: Codable, Hashable, Sendable {
    public var id: String
    public var device: String
    public var chain: Int
    /// starting, active, ended, dismissed, never-started or dead.
    public var state: String
    public var endReason: String?

    enum CodingKeys: String, CodingKey {
        case id, device, chain, state
        case endReason = "end_reason"
    }
}

/// The body of `PUT /v1/activity-instances/{id}`.
public struct InstanceReport: Codable, Hashable, Sendable {
    public var device: String?
    public var token: String?
    /// dismissed or ended.
    public var state: String?

    public init(device: String? = nil, token: String? = nil, state: String? = nil) {
        self.device = device
        self.token = token
        self.state = state
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
