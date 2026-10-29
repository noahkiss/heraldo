import Foundation

/// `GET /v1/revisions`, and the vector each stream poke carries: one number
/// per domain of server data. A domain's number changes whenever what its
/// GET returns may have changed. Clients compare the numbers for equality
/// only. `boot` changes on every server start; a new boot means one full
/// refresh.
public struct Revisions: Codable, Equatable, Sendable {
    public var boot: String
    /// Domain name to revision. Values that are not integers are left out,
    /// so a malformed domain counts as missing.
    public var rev: [String: Int64]

    public init(boot: String, rev: [String: Int64]) {
        self.boot = boot
        self.rev = rev
    }

    enum CodingKeys: String, CodingKey { case boot, rev }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        boot = try c.decode(String.self, forKey: .boot)
        rev = try c.decodeIfPresent(LenientVector.self, forKey: .rev)?.values ?? [:]
    }
}

/// The data of a stream `poke`. A server before revisions sends only the
/// cursor; `revisions` is then nil.
public struct Poke: Decodable, Equatable, Sendable {
    public var cursor: String?
    public var boot: String?
    public var rev: [String: Int64]?

    public init(cursor: String? = nil, revisions: Revisions? = nil) {
        self.cursor = cursor
        boot = revisions?.boot
        rev = revisions?.rev
    }

    enum CodingKeys: String, CodingKey { case cursor, boot, rev }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        cursor = try? c.decodeIfPresent(String.self, forKey: .cursor)
        boot = try? c.decodeIfPresent(String.self, forKey: .boot)
        rev = (try? c.decodeIfPresent(LenientVector.self, forKey: .rev))?.values
    }

    /// The vector, when the poke carries one.
    public var revisions: Revisions? {
        guard let boot, let rev else { return nil }
        return Revisions(boot: boot, rev: rev)
    }
}

/// A JSON object of domain to integer that skips values of another type.
struct LenientVector: Decodable {
    var values: [String: Int64] = [:]

    struct Key: CodingKey {
        var stringValue: String
        var intValue: Int? { nil }
        init(stringValue: String) { self.stringValue = stringValue }
        init?(intValue: Int) { nil }
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: Key.self)
        for key in c.allKeys {
            if let n = try? c.decode(Int64.self, forKey: key) { values[key.stringValue] = n }
        }
    }
}

/// One part of the app's state, refreshed by its own call.
public enum SyncPart: String, CaseIterable, Hashable, Sendable {
    /// The history cache: `/v1/events` after the cache's cursor.
    case history
    /// The inbox cursor and the unread ids: `/v1/cursors/inbox`.
    case unread
    /// The incidents: `/v1/state`.
    case state
    case checks, projects, views, held, activities
    /// The assistant's status: `/v1/llm/status`.
    case llm
}

/// What a sync must fetch.
public enum SyncPlan: Equatable, Sendable {
    /// Everything: the first vector, a new boot, Sync now, an old server.
    case full
    /// Only these parts; empty means nothing changed.
    case parts(Set<SyncPart>)

    public var isEmpty: Bool { self == .parts([]) }

    public func union(_ other: Set<SyncPart>) -> SyncPlan {
        switch self {
        case .full: return .full
        case .parts(let p): return .parts(p.union(other))
        }
    }
}

extension Revisions {
    /// Which parts each domain's change refreshes. A domain not named here
    /// is ignored. A new event also moves the unread count, so `events`
    /// refreshes the unread state too. Mutes and settings have no read of
    /// their own in the app: a mute can change the incidents shown, and
    /// `badge.scope` changes the badge on the cursor.
    public static let parts: [String: Set<SyncPart>] = [
        "events": [.history, .unread],
        "cursor": [.unread],
        "state": [.state],
        "checks": [.checks],
        "projects": [.projects],
        "views": [.views],
        "held": [.held],
        "activities": [.activities],
        "llm": [.llm],
        "mutes": [.state],
        "settings": [.state, .unread],
    ]

    /// What to fetch to go from the vector last applied to this one.
    ///
    /// - No vector applied yet, or another boot: everything.
    /// - A known domain whose number differs: its parts.
    /// - A known domain missing from this vector is unknown: its parts are
    ///   fetched on any poke, and whenever another domain changed.
    public func plan(since applied: Revisions?, poke: Bool = false) -> SyncPlan {
        guard let applied, applied.boot == boot else { return .full }
        var out = Set<SyncPart>()
        var missing = Set<SyncPart>()
        for (domain, parts) in Self.parts {
            guard let now = rev[domain] else {
                missing.formUnion(parts)
                continue
            }
            if applied.rev[domain] != now { out.formUnion(parts) }
        }
        if poke || !out.isEmpty || applied.rev != rev { out.formUnion(missing) }
        return .parts(out)
    }
}
