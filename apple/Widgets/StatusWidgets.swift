#if os(iOS)
import AppIntents
import HeraldoKit
import SwiftUI
import WidgetKit

// The Lock Screen widget (open incidents and down checks) and the
// per-project widget. Both read the snapshot the app saves in the app group
// after each sync, and read the server themselves when the shared
// credentials let them. The app and the notification service extension
// reload them after a sync and on every push.

// MARK: Data

struct StatusEntry: TimelineEntry {
    let date: Date
    /// Nil before the app has synced once on this device.
    let snapshot: WidgetSnapshot?
    /// The project a project widget shows; nil for the Lock Screen widget.
    var project: String?

    static let sample = StatusEntry(date: .now, snapshot: WidgetSnapshot(
        incidents: [.init(id: "sample", title: "Backup failed", project: "homelab")],
        down: [.init(id: "nightly-sync", title: "nightly-sync", project: "homelab")],
        projects: ["homelab"],
        recent: [.init(id: "r1", title: "Disk 80% full", project: "homelab", date: .now)]))
}

enum SnapshotSource {
    /// How often a widget asks again with no push in between.
    static let interval: TimeInterval = 15 * 60

    /// The saved snapshot, brought up to date from the server when it answers in time.
    static func current() async -> WidgetSnapshot? {
        let saved = WidgetSnapshot.load()
        guard let client = try? HeraldoClient(credentials: CredentialStore().load(), session: session) else {
            return saved
        }
        guard let fresh = try? await (saved ?? WidgetSnapshot()).refreshed(with: client) else { return saved }
        try? fresh.save()
        return fresh
    }

    private static let session: URLSession = {
        let c = URLSessionConfiguration.ephemeral
        c.timeoutIntervalForRequest = 8
        c.timeoutIntervalForResource = 12
        return URLSession(configuration: c)
    }()
}

// MARK: Lock Screen widget

struct AttentionProvider: TimelineProvider {
    func placeholder(in context: Context) -> StatusEntry { .sample }

    func getSnapshot(in context: Context, completion: @escaping (StatusEntry) -> Void) {
        if context.isPreview { return completion(.sample) }
        completion(StatusEntry(date: .now, snapshot: WidgetSnapshot.load()))
    }

    func getTimeline(in context: Context, completion: @escaping (Timeline<StatusEntry>) -> Void) {
        Task {
            let entry = StatusEntry(date: .now, snapshot: await SnapshotSource.current())
            completion(Timeline(entries: [entry], policy: .after(.now.addingTimeInterval(SnapshotSource.interval))))
        }
    }
}

/// Open incidents and down checks on the Lock Screen.
struct AttentionWidget: Widget {
    var body: some WidgetConfiguration {
        StaticConfiguration(kind: WidgetKinds.attention, provider: AttentionProvider()) { entry in
            AttentionView(entry: entry)
        }
        .configurationDisplayName("Needs attention")
        .description("Open incidents and down checks.")
        .supportedFamilies([.accessoryCircular, .accessoryRectangular, .accessoryInline])
    }
}

struct AttentionView: View {
    @Environment(\.widgetFamily) private var family
    let entry: StatusEntry

    private var incidents: Int { entry.snapshot?.incidents.count ?? 0 }
    private var down: Int { entry.snapshot?.down.count ?? 0 }
    private var total: Int { incidents + down }

    var body: some View {
        content
            .widgetURL(entry.snapshot?.incidents.first.flatMap { URL(string: "heraldo://event/\($0.id)") })
            .containerBackground(for: .widget) {
                if family == .accessoryCircular { AccessoryWidgetBackground() }
            }
    }

    @ViewBuilder private var content: some View {
        switch family {
        case .accessoryCircular:
            VStack(spacing: 0) {
                Image(systemName: total == 0 ? "checkmark" : "exclamationmark.triangle.fill")
                    .font(.caption)
                if total > 0 { Text("\(total)").font(.title3.bold()).minimumScaleFactor(0.5) }
            }
            .widgetAccentable()
        case .accessoryInline:
            if entry.snapshot == nil {
                Text("Heraldo")
            } else if total == 0 {
                Label("All clear", systemImage: "checkmark")
            } else {
                Label(Counts.line(incidents: incidents, down: down), systemImage: "exclamationmark.triangle.fill")
            }
        default:
            VStack(alignment: .leading, spacing: 1) {
                if entry.snapshot == nil {
                    Text("Heraldo").font(.headline)
                    Text("Open the app to set up").font(.caption)
                } else if total == 0 {
                    Label("All clear", systemImage: "checkmark").font(.headline).widgetAccentable()
                    Text("No open incidents or down checks").font(.caption).lineLimit(2)
                } else {
                    Text(Counts.line(incidents: incidents, down: down)).font(.headline).widgetAccentable()
                    ForEach(lines, id: \.self) { Text($0).font(.caption).lineLimit(1) }
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    /// The first incident and the first down check, or two of one kind.
    private var lines: [String] {
        let s = entry.snapshot
        let titles = (s?.incidents.map(\.title) ?? []) + (s?.down.map { "\($0.title) down" } ?? [])
        return Array(titles.prefix(2))
    }
}

enum Counts {
    static func line(incidents: Int, down: Int) -> String {
        var parts: [String] = []
        if incidents > 0 { parts.append(incidents == 1 ? "1 incident" : "\(incidents) incidents") }
        if down > 0 { parts.append("\(down) down") }
        return parts.isEmpty ? "All clear" : parts.joined(separator: " · ")
    }
}

// MARK: Project widget

/// A project, as the widget's configuration offers it: the projects the app
/// saw at its last sync.
struct ProjectEntity: AppEntity {
    static let typeDisplayRepresentation: TypeDisplayRepresentation = "Project"
    static let defaultQuery = ProjectQuery()

    var id: String
    var displayRepresentation: DisplayRepresentation { DisplayRepresentation(title: "\(id)") }
}

struct ProjectQuery: EntityQuery {
    func entities(for identifiers: [String]) async throws -> [ProjectEntity] {
        identifiers.map(ProjectEntity.init(id:))
    }

    func suggestedEntities() async throws -> [ProjectEntity] {
        (WidgetSnapshot.load()?.projects ?? []).map(ProjectEntity.init(id:))
    }
}

struct ProjectWidgetIntent: WidgetConfigurationIntent {
    static let title: LocalizedStringResource = "Project"
    static let description = IntentDescription("Shows one project's open incidents, down checks and newest events.")

    @Parameter(title: "Project")
    var project: ProjectEntity?
}

struct ProjectProvider: AppIntentTimelineProvider {
    func placeholder(in context: Context) -> StatusEntry {
        var e = StatusEntry.sample
        e.project = "homelab"
        return e
    }

    func snapshot(for configuration: ProjectWidgetIntent, in context: Context) async -> StatusEntry {
        if context.isPreview { return placeholder(in: context) }
        return entry(WidgetSnapshot.load(), configuration)
    }

    func timeline(for configuration: ProjectWidgetIntent, in context: Context) async -> Timeline<StatusEntry> {
        let e = entry(await SnapshotSource.current(), configuration)
        return Timeline(entries: [e], policy: .after(.now.addingTimeInterval(SnapshotSource.interval)))
    }

    private func entry(_ s: WidgetSnapshot?, _ c: ProjectWidgetIntent) -> StatusEntry {
        let p = c.project?.id
        return StatusEntry(date: .now, snapshot: p.flatMap { s?.project($0) }, project: p)
    }
}

/// One project's open incidents, down checks and newest events.
struct ProjectWidget: Widget {
    var body: some WidgetConfiguration {
        AppIntentConfiguration(kind: WidgetKinds.project, intent: ProjectWidgetIntent.self,
                               provider: ProjectProvider()) { entry in
            ProjectView(entry: entry)
        }
        .configurationDisplayName("Project")
        .description("One project's open incidents, down checks and newest events.")
        .supportedFamilies([.systemSmall, .systemMedium, .accessoryRectangular])
    }
}

struct ProjectView: View {
    @Environment(\.widgetFamily) private var family
    let entry: StatusEntry

    private var s: WidgetSnapshot { entry.snapshot ?? WidgetSnapshot() }

    var body: some View {
        content
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            .widgetURL(s.incidents.first.flatMap { URL(string: "heraldo://event/\($0.id)") })
            .containerBackground(.fill.tertiary, for: .widget)
    }

    @ViewBuilder private var content: some View {
        if entry.project == nil || entry.snapshot == nil {
            VStack(alignment: .leading) {
                Text(entry.project ?? "Project").font(.headline)
                Text(entry.project == nil ? "Edit the widget to choose a project." : "Open the app to set up")
                    .font(.caption).foregroundStyle(.secondary)
            }
        } else if family == .accessoryRectangular {
            VStack(alignment: .leading, spacing: 1) {
                Text(entry.project ?? "").font(.headline).lineLimit(1).widgetAccentable()
                Text(Counts.line(incidents: s.incidents.count, down: s.down.count)).font(.caption)
                if let first = s.incidents.first ?? s.recent.first {
                    Text(first.title).font(.caption).lineLimit(1)
                }
            }
        } else {
            VStack(alignment: .leading, spacing: 4) {
                HStack {
                    Text(entry.project ?? "").font(.headline).lineLimit(1)
                    Spacer(minLength: 0)
                    if s.incidents.count + s.down.count > 0 {
                        Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
                    }
                }
                Text(Counts.line(incidents: s.incidents.count, down: s.down.count))
                    .font(.subheadline)
                    .foregroundStyle(s.incidents.count + s.down.count > 0 ? .primary : .secondary)
                Divider()
                ForEach(rows) { item in
                    Text(item.title).font(.caption).lineLimit(1)
                }
                if rows.isEmpty {
                    Text("Nothing new").font(.caption).foregroundStyle(.secondary)
                }
                Spacer(minLength: 0)
            }
        }
    }

    /// Incidents first, then down checks, then the newest events.
    private var rows: [WidgetSnapshot.Item] {
        let all = s.incidents + s.down.map { WidgetSnapshot.Item(id: $0.id, title: "\($0.title) down") } + s.recent
        return Array(all.prefix(family == .systemMedium ? 4 : 3))
    }
}
#endif
