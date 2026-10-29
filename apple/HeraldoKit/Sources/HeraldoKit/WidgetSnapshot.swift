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
        /// An incident's key, so a later event of the incident can replace
        /// or remove its line. Nil in snapshots written before it.
        public var key: String?

        public init(id: String, title: String, project: String? = nil, date: Date? = nil, level: String? = nil,
                    key: String? = nil) {
            self.id = id
            self.title = title
            self.project = project
            self.date = date
            self.level = level
            self.key = key
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
            .map { Item(id: $0.id, title: $0.headline, project: $0.project, date: $0.date, level: $0.level, key: $0.key) }
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

    // MARK: One event

    /// Takes one pushed event into the snapshot, as the notification
    /// extension sees it, and returns the widget kinds whose content changed
    /// (empty when nothing did). `updated` moves only on a change.
    ///
    /// - An open or updated incident, not acked: its line, newest first,
    ///   replacing an older line of the same key. Acked or resolved: removed.
    /// - The resolve of a check's incident (`checkin:<key>`): the check
    ///   leaves the down list.
    /// - A failing check-in: the check joins the down list.
    /// - Any event that is not routine and names a project: a recent line.
    @discardableResult
    public mutating func apply(_ e: Event, at date: Date = .now) -> Set<String> {
        let before = self
        if e.kind == "incident", let key = e.key, !key.isEmpty {
            incidents.removeAll { $0.key == key || $0.id == e.id }
            if e.isOpenIncident && !e.isAcked {
                incidents.insert(Item(id: e.id, title: e.headline, project: e.project, date: e.date,
                                      level: e.level, key: key), at: 0)
                incidents.sort { ($0.date ?? .distantPast) > ($1.date ?? .distantPast) }
            }
            if key.hasPrefix("checkin:"), e.action == "resolve" {
                let check = String(key.dropFirst("checkin:".count))
                down.removeAll { $0.id == check }
            }
        }
        if e.isFailingCheckin, let key = e.key, !key.isEmpty, !down.contains(where: { $0.id == key }) {
            down.append(Item(id: key, title: key, project: e.project, date: e.date))
        }
        if !e.isRoutine, let p = e.project, !p.isEmpty, !recent.contains(where: { $0.id == e.id }) {
            recent.insert(Item(id: e.id, title: e.headline, project: p, date: e.date, level: e.level), at: 0)
            var kept: [String: Int] = [:]
            recent = recent.filter { item in
                guard let ip = item.project else { return false }
                kept[ip, default: 0] += 1
                return kept[ip]! <= Self.recentLimit
            }
            if !projects.contains(p) {
                projects.append(p)
                projects.sort { $0.localizedCaseInsensitiveCompare($1) == .orderedAscending }
            }
        }
        let kinds = changedKinds(from: before)
        if !kinds.isEmpty { updated = date }
        return kinds
    }

    /// The widget kinds whose content differs from `old` (nil: all of
    /// them). Needs attention shows the incidents and down checks; Project
    /// shows all of it. `updated` does not count.
    public func changedKinds(from old: WidgetSnapshot?) -> Set<String> {
        guard let old else { return Set(WidgetKinds.all) }
        var out = Set<String>()
        if incidents != old.incidents || down != old.down {
            out.insert(WidgetKinds.attention)
            out.insert(WidgetKinds.project)
        }
        if recent != old.recent || projects != old.projects { out.insert(WidgetKinds.project) }
        return out
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
