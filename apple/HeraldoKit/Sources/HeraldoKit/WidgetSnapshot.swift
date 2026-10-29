import Foundation

/// What the widgets show: open incidents, down checks and, per project, the
/// newest events. The app writes it to the app group after each sync; a
/// widget refreshes the incidents and checks itself when it can reach the
/// server, and falls back to this copy when it cannot.
public struct WidgetSnapshot: Codable, Equatable, Sendable {
    public var updated: Date
    /// Open incidents not yet acknowledged, newest first.
    public var incidents: [Item]
    /// Checks that are down, the longest down first.
    public var down: [Item]
    /// The projects the app knows, sorted, for the widget's project picker.
    public var projects: [String]
    /// The newest events that are not routine, newest first, at most
    /// `recentLimit` per project. Events with no project are left out.
    public var recent: [Item]

    public static let recentLimit = 3

    /// One line of a widget.
    public struct Item: Codable, Equatable, Sendable, Identifiable {
        public var id: String
        public var title: String
        public var project: String?
        public var date: Date?
        public var level: String?

        public init(id: String, title: String, project: String? = nil, date: Date? = nil, level: String? = nil) {
            self.id = id
            self.title = title
            self.project = project
            self.date = date
            self.level = level
        }
    }

    public init(updated: Date = .now, incidents: [Item] = [], down: [Item] = [], projects: [String] = [],
                recent: [Item] = []) {
        self.updated = updated
        self.incidents = incidents
        self.down = down
        self.projects = projects
        self.recent = recent
    }

    /// The snapshot of a sync's results. `events` is the cache, newest first.
    public init(incidents: [Event], checks: [Check], projects: [String], events: [Event], at date: Date = .now) {
        self.updated = date
        self.incidents = Self.items(incidents: incidents)
        self.down = Self.items(checks: checks)
        self.projects = projects
        var perProject: [String: Int] = [:]
        var recent: [Item] = []
        for e in events where !e.isRoutine {
            guard let p = e.project, perProject[p, default: 0] < Self.recentLimit else { continue }
            perProject[p, default: 0] += 1
            recent.append(Item(id: e.id, title: e.headline, project: p, date: e.date, level: e.level))
        }
        self.recent = recent
    }

    /// The incidents not acknowledged or resolved, as widget lines, newest
    /// first: the app's Incidents badge.
    public static func items(incidents: [Event]) -> [Item] {
        incidents.filter { !$0.isAcked && $0.action != "resolve" }
            .sorted { ($0.date ?? .distantPast) > ($1.date ?? .distantPast) }
            .map { Item(id: $0.id, title: $0.headline, project: $0.project, date: $0.date, level: $0.level) }
    }

    /// The down checks as widget lines, the longest down first.
    public static func items(checks: [Check]) -> [Item] {
        checks.filter { $0.state == .down }
            .sorted { ($0.stateSince ?? .distantPast) < ($1.stateSince ?? .distantPast) }
            .map { Item(id: $0.key, title: $0.key, project: $0.project, date: $0.stateSince) }
    }

    /// The snapshot narrowed to one project.
    public func project(_ name: String) -> WidgetSnapshot {
        WidgetSnapshot(updated: updated, incidents: incidents.filter { $0.project == name },
                       down: down.filter { $0.project == name }, projects: projects,
                       recent: recent.filter { $0.project == name })
    }

    /// This snapshot with the incidents and checks read from the server
    /// now. The projects and recent events stay as the app saved them.
    public func refreshed(with client: HeraldoClient, at date: Date = .now) async throws -> WidgetSnapshot {
        let state = try await client.state()
        let checks = try await client.checks() ?? []
        var s = self
        s.updated = date
        s.incidents = Self.items(incidents: state.incidents)
        s.down = Self.items(checks: checks)
        return s
    }

    // MARK: File

    /// The file in the app group's folder, or nil when the build has no group.
    public static func url(_ bundle: Bundle = .main) -> URL? {
        AppGroup.container(bundle)?.appending(path: "widgets.json")
    }

    public static func load(_ bundle: Bundle = .main) -> WidgetSnapshot? {
        guard let url = url(bundle), let data = try? Data(contentsOf: url) else { return nil }
        return try? JSON.decoder.decode(WidgetSnapshot.self, from: data)
    }

    public func save(_ bundle: Bundle = .main) throws {
        guard let url = Self.url(bundle) else { return }
        try JSON.encoder.encode(self).write(to: url, options: .atomic)
    }
}

/// The widget kinds, for `WidgetCenter.reloadTimelines(ofKind:)`.
public enum WidgetKinds {
    public static let attention = "attention"
    public static let project = "project"
    public static let all = [attention, project]
}
