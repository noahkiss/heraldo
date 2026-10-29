import Foundation

// The running Live Activities and the alert hold's queue: two of the Mac
// app's homes.

/// An activity's content as the read API returns it: the envelope's
/// LiveContent, with `ends_at` as RFC 3339 text. `state` turns it into the
/// pushed form, for its progress helpers.
public struct ActivityContent: Codable, Hashable, Sendable {
    public var title: String?
    public var symbol: String?
    public var tint: String?
    public var body: String?
    public var progress: Double?
    public var steps: LiveSteps?
    public var endsAt: Date?
    public var trailing: String?
    public var status: String?
    public var metrics: [LiveMetric]?

    enum CodingKeys: String, CodingKey {
        case title, symbol, tint, body, progress, steps, trailing, status, metrics
        case endsAt = "ends_at"
    }

    public init(title: String? = nil, body: String? = nil, progress: Double? = nil, status: String? = nil) {
        self.title = title
        self.body = body
        self.progress = progress
        self.status = status
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        title = try? c.decodeIfPresent(String.self, forKey: .title)
        symbol = try? c.decodeIfPresent(String.self, forKey: .symbol)
        tint = try? c.decodeIfPresent(String.self, forKey: .tint)
        body = try? c.decodeIfPresent(String.self, forKey: .body)
        progress = try? c.decodeIfPresent(Double.self, forKey: .progress)
        steps = try? c.decodeIfPresent(LiveSteps.self, forKey: .steps)
        if let s = try? c.decodeIfPresent(String.self, forKey: .endsAt) { endsAt = JSON.parseDate(s) }
        trailing = try? c.decodeIfPresent(String.self, forKey: .trailing)
        status = try? c.decodeIfPresent(String.self, forKey: .status)
        // A metric the app cannot read is left out, not the whole content.
        if let list = try? c.decodeIfPresent([Lenient<LiveMetric>].self, forKey: .metrics) {
            metrics = list.compactMap(\.value)
        }
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encodeIfPresent(title, forKey: .title)
        try c.encodeIfPresent(symbol, forKey: .symbol)
        try c.encodeIfPresent(tint, forKey: .tint)
        try c.encodeIfPresent(body, forKey: .body)
        try c.encodeIfPresent(progress, forKey: .progress)
        try c.encodeIfPresent(steps, forKey: .steps)
        try c.encodeIfPresent(endsAt.map(JSON.formatDate), forKey: .endsAt)
        try c.encodeIfPresent(trailing, forKey: .trailing)
        try c.encodeIfPresent(status, forKey: .status)
        try c.encodeIfPresent(metrics, forKey: .metrics)
    }

    /// The pushed form, for `fraction`, `progressText` and `shownMetrics`.
    public var state: LiveState {
        LiveState(title: title, symbol: symbol, tint: tint, body: body, progress: progress, steps: steps,
                  endsAt: endsAt, trailing: trailing, status: status, metrics: metrics)
    }
}

/// One entry of `GET /v1/activities`: an activity and its content.
public struct ActivitySummary: Codable, Hashable, Identifiable, Sendable {
    public var key: String
    /// act_ plus the id of the event that began it.
    public var activityID: String
    /// active or ended.
    public var state: String
    public var source: String
    public var content: ActivityContent?
    public var startedAt: Date?
    public var updatedAt: Date?
    public var endedAt: Date?
    public var endReason: String?
    public var instances: [LiveInstance]?

    /// The key: one activity runs on a key at a time.
    public var id: String { key }

    enum CodingKeys: String, CodingKey {
        case key, state, source, content, instances
        case activityID = "id"
        case startedAt = "started_at"
        case updatedAt = "updated_at"
        case endedAt = "ended_at"
        case endReason = "end_reason"
    }

    public init(key: String, activityID: String, state: String = "active", source: String,
                content: ActivityContent? = nil, startedAt: Date? = nil, updatedAt: Date? = nil) {
        self.key = key
        self.activityID = activityID
        self.state = state
        self.source = source
        self.content = content
        self.startedAt = startedAt
        self.updatedAt = updatedAt
    }

    /// The content's title, else the key.
    public var headline: String {
        if let t = content?.title, !t.isEmpty { return t }
        return key
    }
}

public struct ActivityList: Codable, Sendable {
    public var activities: [ActivitySummary]
}

/// One entry of the alert hold's queue, `GET /v1/hold`.
public struct HoldEntry: Codable, Hashable, Identifiable, Sendable {
    public var id: String
    public var eventID: String?
    /// When it was held, in epoch seconds.
    public var ts: Double
    public var deadlineTs: Double?
    public var closedTs: Double?
    public var tag: String?
    public var title: String?
    public var body: String?
    public var client: String?
    public var source: String?
    public var forwarded: Bool?
    public var shadow: Bool?
    /// held, released, suppressed or expired.
    public var status: String
    public var verdict: String?
    public var reason: String?

    enum CodingKeys: String, CodingKey {
        case id, ts, tag, title, body, client, source, forwarded, shadow, status, verdict, reason
        case eventID = "event_id"
        case deadlineTs = "deadline_ts"
        case closedTs = "closed_ts"
    }

    public init(id: String, ts: Double, status: String = "held", title: String? = nil, body: String? = nil,
                source: String? = nil) {
        self.id = id
        self.ts = ts
        self.status = status
        self.title = title
        self.body = body
        self.source = source
    }

    public var date: Date { Date(timeIntervalSince1970: ts) }
    public var deadline: Date? { deadlineTs.map(Date.init(timeIntervalSince1970:)) }
    public var closed: Date? { closedTs.map(Date.init(timeIntervalSince1970:)) }
    /// Still held: not released, suppressed or expired.
    public var isOpen: Bool { status == "held" }

    /// The title, else the body's first line, else the id.
    public var headline: String {
        if let t = title, !t.isEmpty { return t }
        if let line = body?.split(separator: "\n").first, !line.isEmpty { return String(line) }
        return id
    }
}

/// `GET /v1/hold`: the hold's mode and its entries.
public struct HoldQueue: Codable, Sendable {
    /// off, shadow or on.
    public var mode: String
    public var held: [HoldEntry]
}
