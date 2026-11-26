import Foundation

/// What a delivery setting does with a matching event (docs/api.md,
/// "Delivery settings").
public enum DeliverOutcome: String, Codable, CaseIterable, Hashable, Sendable {
    case banner, silent, digest, drop

    public var label: String {
        switch self {
        case .banner: return "Banner"
        case .silent: return "Silent"
        case .digest: return "Digest"
        case .drop: return "Drop"
        }
    }

    /// Hidden from the inbox (`hidden=exclude`).
    public var isHidden: Bool { self == .digest || self == .drop }
}

/// Whether a matching event counts toward the badge.
public enum DeliveryBadge: String, Codable, CaseIterable, Hashable, Sendable {
    /// `badge.scope` decides.
    case auto
    case on
    case off
}

/// The delivery setting's decision, stamped on a stored event at ingest
/// (`deliver`). Every field is a plain string, so a value a later server
/// adds never fails the decode of a whole page.
public struct DeliverStamp: Codable, Hashable, Sendable {
    /// `as`: banner, silent, digest or drop.
    public var applied: String?
    /// The id of the row that matched; nil when only a level was filled.
    public var by: String?
    public var levelWas: String?
    public var badge: String?
    /// The outcome the row asked for when the server applied another.
    public var wanted: String?
    /// The floor that refused the row's outcome.
    public var refused: String?
    /// The level the server filled for an event that carried none.
    public var filled: String?
    public var newSource: Bool?

    enum CodingKeys: String, CodingKey {
        case applied = "as"
        case by, badge, wanted, refused, filled
        case levelWas = "level_was"
        case newSource = "new_source"
    }

    public init(applied: String? = nil, by: String? = nil, levelWas: String? = nil, badge: String? = nil,
                wanted: String? = nil, refused: String? = nil, filled: String? = nil, newSource: Bool? = nil) {
        self.applied = applied
        self.by = by
        self.levelWas = levelWas
        self.badge = badge
        self.wanted = wanted
        self.refused = refused
        self.filled = filled
        self.newSource = newSource
    }

    public var outcome: DeliverOutcome? { applied.flatMap(DeliverOutcome.init(rawValue:)) }
    public var wantedOutcome: DeliverOutcome? { wanted.flatMap(DeliverOutcome.init(rawValue:)) }
    public var badgeChoice: DeliveryBadge? { badge.flatMap(DeliveryBadge.init(rawValue:)) }

    /// Filed for the digest or dropped: the inbox leaves it out. Only the
    /// applied outcome counts; a `wanted: digest` acted as silent.
    public var isHidden: Bool { outcome?.isHidden ?? false }

    /// One line for an event's detail, such as "Silent by Backups noise
    /// (was active) · badge off". `note` names the row when the app knows it.
    public func summary(note: String? = nil) -> String {
        var head: String
        switch outcome {
        case .banner?: head = "Delivered as a banner"
        case .silent?: head = "Delivered silently"
        case .digest?: head = "Filed for the digest"
        case .drop?: head = "Dropped"
        case nil: head = "Delivery \(applied ?? "unknown")"
        }
        if let note, !note.isEmpty {
            head += " by \"\(note)\""
        } else if by != nil {
            head += " by a delivery setting"
        }
        var parts = [head]
        if let levelWas, outcome != .banner { parts.append("was \(levelWas)") }
        if let wanted, wanted != applied {
            if let refused, !refused.isEmpty {
                parts.append("wanted \(wanted), refused by the \(refused)")
            } else if wanted == DeliverOutcome.digest.rawValue {
                parts.append("wanted digest; the digest is off")
            } else {
                parts.append("wanted \(wanted)")
            }
        }
        if let filled { parts.append("level filled as \(filled)") }
        if newSource == true { parts.append("new source") }
        if let badge, badge != DeliveryBadge.auto.rawValue { parts.append("badge \(badge)") }
        return parts.joined(separator: " · ")
    }
}

extension Event {
    /// Filed for the digest or dropped by a delivery setting.
    public var isHidden: Bool { deliver?.isHidden ?? false }
}

/// Which events a delivery setting matches. Every field set must match; an
/// empty match matches every event that is not routine.
public struct DeliveryMatch: Codable, Hashable, Sendable {
    public var source: String?
    public var tag: String?
    public var level: [String]?
    public var kind: String?
    public var project: String?
    public var rule: String?
    /// A key prefix.
    public var key: String?

    public init(source: String? = nil, tag: String? = nil, level: [String]? = nil, kind: String? = nil,
                project: String? = nil, rule: String? = nil, key: String? = nil) {
        self.source = source
        self.tag = tag
        self.level = level
        self.kind = kind
        self.project = project
        self.rule = rule
        self.key = key
    }

    /// The same match with empty strings and lists cleared, as the server
    /// stores it.
    public var normalized: DeliveryMatch {
        func s(_ v: String?) -> String? {
            let t = v?.trimmingCharacters(in: .whitespaces) ?? ""
            return t.isEmpty ? nil : t
        }
        return DeliveryMatch(source: s(source), tag: s(tag), level: (level ?? []).isEmpty ? nil : level,
                             kind: s(kind), project: s(project), rule: s(rule), key: s(key))
    }

    public var isEmpty: Bool { normalized == DeliveryMatch() }

    /// "source backup · level passive, active", or "every event".
    public var summary: String {
        let m = normalized
        var parts: [String] = []
        if let v = m.source { parts.append("source \(v)") }
        if let v = m.tag { parts.append("tag \(v)") }
        if let v = m.level { parts.append("level \(v.joined(separator: ", "))") }
        if let v = m.kind { parts.append("kind \(v)") }
        if let v = m.project { parts.append("project \(v)") }
        if let v = m.rule { parts.append("rule \(v)") }
        if let v = m.key { parts.append("key \(v)…") }
        return parts.isEmpty ? "Every event" : parts.joined(separator: " · ")
    }
}

/// The body of `POST /v1/delivery`, `PUT /v1/delivery/{id}`, and one row of
/// a preview. PUT replaces every field: one left out goes back to its default.
public struct DeliverySettingInput: Codable, Hashable, Sendable {
    public var match: DeliveryMatch
    public var deliver: DeliverOutcome?
    public var badge: DeliveryBadge?
    public var defaultLevel: String?
    public var position: Int?
    public var note: String?

    enum CodingKeys: String, CodingKey {
        case match, deliver, badge, position, note
        case defaultLevel = "default_level"
    }

    public init(match: DeliveryMatch = DeliveryMatch(), deliver: DeliverOutcome? = nil, badge: DeliveryBadge? = nil,
                defaultLevel: String? = nil, position: Int? = nil, note: String? = nil) {
        self.match = match
        self.deliver = deliver
        self.badge = badge
        self.defaultLevel = defaultLevel
        self.position = position
        self.note = note
    }

    /// The input that stores `row` again unchanged.
    public init(_ row: DeliverySetting) {
        self.init(match: row.match, deliver: row.deliver, badge: row.badge, defaultLevel: row.defaultLevel,
                  position: row.position, note: row.note)
    }

    /// The same input with empty fields left out, as the server wants it.
    public var normalized: DeliverySettingInput {
        var c = self
        c.match = match.normalized
        if c.badge == .auto { c.badge = nil }
        if (c.defaultLevel ?? "").isEmpty { c.defaultLevel = nil }
        let n = note?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        c.note = n.isEmpty ? nil : n
        return c
    }

    /// Why the server would refuse this row, or nil. The same checks as the
    /// server's, so the editor can say so before saving.
    public var problem: String? {
        let r = normalized
        if r.deliver == nil, r.badge == nil, r.defaultLevel == nil {
            return "The row does nothing: choose an outcome, a badge or a default level."
        }
        if r.defaultLevel != nil, r.match.level != nil {
            return "A row with a default level may not match on level."
        }
        if r.badge == .on, let d = r.deliver, d.isHidden {
            return "Badge on with \(d.rawValue) would count events the inbox hides."
        }
        if let k = r.match.kind, k == "progress" || k == "status" {
            return "\(k.capitalized) events are routine: delivery settings never apply to them."
        }
        return nil
    }
}

/// One delivery setting from `GET /v1/delivery`.
public struct DeliverySetting: Codable, Hashable, Identifiable, Sendable {
    public var id: String
    public var match: DeliveryMatch
    public var deliver: DeliverOutcome?
    public var badge: DeliveryBadge
    public var defaultLevel: String?
    public var position: Int
    public var note: String?
    public var createdAt: Date?
    public var updatedAt: Date?
    public var updatedBy: String?

    enum CodingKeys: String, CodingKey {
        case id, match, deliver, badge, position, note
        case defaultLevel = "default_level"
        case createdAt = "created_at"
        case updatedAt = "updated_at"
        case updatedBy = "updated_by"
    }

    public init(id: String, match: DeliveryMatch = DeliveryMatch(), deliver: DeliverOutcome? = nil,
                badge: DeliveryBadge = .auto, defaultLevel: String? = nil, position: Int = 0, note: String? = nil) {
        self.id = id
        self.match = match
        self.deliver = deliver
        self.badge = badge
        self.defaultLevel = defaultLevel
        self.position = position
        self.note = note
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        match = try c.decodeIfPresent(DeliveryMatch.self, forKey: .match) ?? DeliveryMatch()
        deliver = try c.decodeIfPresent(String.self, forKey: .deliver).flatMap(DeliverOutcome.init(rawValue:))
        badge = try c.decodeIfPresent(String.self, forKey: .badge).flatMap(DeliveryBadge.init(rawValue:)) ?? .auto
        defaultLevel = try c.decodeIfPresent(String.self, forKey: .defaultLevel)
        position = try c.decodeIfPresent(Int.self, forKey: .position) ?? 0
        note = try c.decodeIfPresent(String.self, forKey: .note)
        createdAt = try c.decodeIfPresent(Date.self, forKey: .createdAt)
        updatedAt = try c.decodeIfPresent(Date.self, forKey: .updatedAt)
        updatedBy = try c.decodeIfPresent(String.self, forKey: .updatedBy)
    }

    /// What the row does, in one line: "Silent · badge off · fills passive".
    public var effect: String {
        var parts: [String] = []
        if let deliver { parts.append(deliver.label) }
        if badge != .auto { parts.append("badge \(badge.rawValue)") }
        if let defaultLevel { parts.append("fills \(defaultLevel)") }
        return parts.isEmpty ? "No change" : parts.joined(separator: " · ")
    }
}

/// `GET /v1/delivery`.
public struct DeliverySettingList: Codable, Hashable, Sendable {
    /// `delivery.enabled`: while off, the rows do nothing.
    public var enabled: Bool
    public var newSourceLevel: String?
    public var settings: [DeliverySetting]

    enum CodingKeys: String, CodingKey {
        case enabled, settings
        case newSourceLevel = "new_source_level"
    }

    public init(enabled: Bool, newSourceLevel: String? = nil, settings: [DeliverySetting]) {
        self.enabled = enabled
        self.newSourceLevel = newSourceLevel
        self.settings = settings
    }
}

/// The body of `POST /v1/delivery/preview`.
public struct DeliveryPreviewInput: Codable, Hashable, Sendable {
    /// The rows to try, in order: the first match wins, so a preview of one
    /// edited row sends the whole list with it in place.
    public var settings: [DeliverySettingInput]
    public var days: Int?
    public var limit: Int?

    public init(settings: [DeliverySettingInput], days: Int? = nil, limit: Int? = nil) {
        self.settings = settings
        self.days = days
        self.limit = limit
    }
}

/// `POST /v1/delivery/preview`: what the rows would change over the stored
/// events of the last days, against the same events with no rows.
public struct DeliveryPreview: Codable, Hashable, Sendable {
    public struct Outcomes: Codable, Hashable, Sendable {
        public var banner: Int
        public var silent: Int
        public var digest: Int
        public var drop: Int
        public var unchanged: Int
    }

    public struct Row: Codable, Hashable, Sendable {
        /// The row's place in the settings sent, from 0.
        public var index: Int
        public var hits: Int
        /// Hits a floor kept at their own level.
        public var refused: Int
    }

    public struct Change: Codable, Hashable, Identifiable, Sendable {
        public var id: String
        public var source: String
        public var title: String?
        public var kind: String?
        public var applied: String?
        public var row: Int?
        public var refused: String?
        public var levelBefore: String?
        public var levelAfter: String?
        public var badgeBefore: Bool
        public var badgeAfter: Bool

        enum CodingKeys: String, CodingKey {
            case id, source, title, kind, row, refused
            case applied = "as"
            case levelBefore = "level_before"
            case levelAfter = "level_after"
            case badgeBefore = "badge_before"
            case badgeAfter = "badge_after"
        }
    }

    public var days: Int
    /// The events the rows ran over, routine events left out.
    public var events: Int
    public var outcomes: Outcomes
    public var rows: [Row]
    /// Events that would count toward the badge and do not without rows.
    public var badgeJoined: Int
    /// Events that count toward the badge without rows and would not.
    public var badgeLeft: Int
    public var changed: [Change]
    public var truncated: Bool

    enum CodingKeys: String, CodingKey {
        case days, events, outcomes, rows, changed, truncated
        case badgeJoined = "badge_joined"
        case badgeLeft = "badge_left"
    }

    /// The hits of the row at `index` in the settings sent.
    public func row(_ index: Int) -> Row? { rows.first { $0.index == index } }
}

/// A source from `GET /v1/sources`, with its last 30 days.
public struct Source: Codable, Hashable, Identifiable, Sendable {
    /// Folded to lower case.
    public var source: String
    public var firstSeen: Date?
    public var lastSeen: Date?
    public var reviewedAt: Date?
    /// Not reviewed yet: its level-less events get `delivery.new_source_level`.
    public var new: Bool
    /// Events in the last 30 days, routine left out.
    public var events: Int
    /// Of those, the ones that counted toward the badge.
    public var badgeIn: Int
    /// Of those, the ones that did not.
    public var badgeOut: Int

    public var id: String { source }

    enum CodingKeys: String, CodingKey {
        case source, new, events
        case firstSeen = "first_seen"
        case lastSeen = "last_seen"
        case reviewedAt = "reviewed_at"
        case badgeIn = "badge_in"
        case badgeOut = "badge_out"
    }

    public init(source: String, new: Bool = false, events: Int = 0, badgeIn: Int = 0, badgeOut: Int = 0,
                firstSeen: Date? = nil, lastSeen: Date? = nil, reviewedAt: Date? = nil) {
        self.source = source
        self.new = new
        self.events = events
        self.badgeIn = badgeIn
        self.badgeOut = badgeOut
        self.firstSeen = firstSeen
        self.lastSeen = lastSeen
        self.reviewedAt = reviewedAt
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        source = try c.decode(String.self, forKey: .source)
        firstSeen = try c.decodeIfPresent(Date.self, forKey: .firstSeen)
        lastSeen = try c.decodeIfPresent(Date.self, forKey: .lastSeen)
        reviewedAt = try c.decodeIfPresent(Date.self, forKey: .reviewedAt)
        new = try c.decodeIfPresent(Bool.self, forKey: .new) ?? false
        events = try c.decodeIfPresent(Int.self, forKey: .events) ?? 0
        badgeIn = try c.decodeIfPresent(Int.self, forKey: .badgeIn) ?? 0
        badgeOut = try c.decodeIfPresent(Int.self, forKey: .badgeOut) ?? 0
    }

    /// New sources first, then by name; the server's order, kept for a
    /// list the app filters or edits.
    public static func ordered(_ list: [Source]) -> [Source] {
        list.sorted { a, b in a.new == b.new ? a.source < b.source : a.new }
    }
}

/// `GET /v1/sources`.
public struct SourceList: Codable, Hashable, Sendable {
    public var sources: [Source]
}
