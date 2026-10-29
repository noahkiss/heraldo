import Foundation

/// A saved view: a named, stored event filter, from `GET /v1/views`.
public struct SavedView: Codable, Hashable, Identifiable, Sendable {
    public var id: String
    public var name: String
    public var filter: ViewFilter
    public var bundle: ViewBundle?
    public var position: Int
    /// An SF Symbol name.
    public var icon: String?
    public var createdAt: Date?
    public var updatedAt: Date?

    enum CodingKeys: String, CodingKey {
        case id, name, filter, bundle, position, icon
        case createdAt = "created_at"
        case updatedAt = "updated_at"
    }

    public init(id: String, name: String, filter: ViewFilter = ViewFilter(), bundle: ViewBundle? = nil,
                position: Int = 0, icon: String? = nil, createdAt: Date? = nil, updatedAt: Date? = nil) {
        self.id = id
        self.name = name
        self.filter = filter
        self.bundle = bundle
        self.position = position
        self.icon = icon
        self.createdAt = createdAt
        self.updatedAt = updatedAt
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        name = try c.decode(String.self, forKey: .name)
        filter = try c.decodeIfPresent(ViewFilter.self, forKey: .filter) ?? ViewFilter()
        bundle = try c.decodeIfPresent(ViewBundle.self, forKey: .bundle)
        position = try c.decodeIfPresent(Int.self, forKey: .position) ?? 0
        icon = try c.decodeIfPresent(String.self, forKey: .icon).flatMap { $0.isEmpty ? nil : $0 }
        createdAt = try c.decodeIfPresent(Date.self, forKey: .createdAt)
        updatedAt = try c.decodeIfPresent(Date.self, forKey: .updatedAt)
    }
}

/// A view's filter. Every field is optional; a nil field matches all.
public struct ViewFilter: Codable, Hashable, Sendable {
    /// Projects; `noProject` matches the events with none.
    public var project: [String]?
    public var source: String?
    public var kind: String?
    public var host: String?
    public var tag: String?
    public var q: String?
    /// Levels: passive, active, time-sensitive, critical.
    public var level: [String]?
    /// `exclude` or `only`; nil keeps routine and other events.
    public var routine: EventQuery.Routine?

    public init(project: [String]? = nil, source: String? = nil, kind: String? = nil, host: String? = nil,
                tag: String? = nil, q: String? = nil, level: [String]? = nil, routine: EventQuery.Routine? = nil) {
        self.project = project
        self.source = source
        self.kind = kind
        self.host = host
        self.tag = tag
        self.q = q
        self.level = level
        self.routine = routine
    }

    /// No field is set.
    public var isEmpty: Bool { self == ViewFilter() }
}

extension EventQuery.Routine: Codable {}

/// How a view bundles a burst of events: a time window and how many show.
public struct ViewBundle: Codable, Hashable, Sendable {
    /// A duration, such as `15m`.
    public var window: String?
    public var maxVisible: Int?

    enum CodingKeys: String, CodingKey {
        case window
        case maxVisible = "max_visible"
    }

    public init(window: String? = nil, maxVisible: Int? = nil) {
        self.window = window
        self.maxVisible = maxVisible
    }
}

/// The body of `POST /v1/views` and `PUT /v1/views/{id}`.
public struct ViewInput: Codable, Hashable, Sendable {
    public var name: String
    public var filter: ViewFilter
    public var bundle: ViewBundle?
    public var position: Int?
    public var icon: String?

    public init(name: String, filter: ViewFilter, bundle: ViewBundle? = nil, position: Int? = nil, icon: String? = nil) {
        self.name = name
        self.filter = filter
        self.bundle = bundle
        self.position = position
        self.icon = icon
    }

    /// The input that stores `view` again unchanged.
    public init(_ view: SavedView) {
        self.init(name: view.name, filter: view.filter, bundle: view.bundle, position: view.position, icon: view.icon)
    }
}

/// `GET /v1/views`.
public struct ViewList: Codable, Sendable {
    public var views: [SavedView]
}
