import HeraldoKit
import SwiftUI

/// The history: the synced cache, or server results while a search or filter is set.
struct InboxView: View {
    @Environment(AppModel.self) private var model
    @State private var path: [Event] = []
    @State private var feed = InboxFeed()

    private var events: [Event] { feed.events(model) }

    var body: some View {
        NavigationStack(path: $path) {
            list
                .navigationTitle(model.views.first { $0.id == feed.viewID }?.name ?? "Inbox")
                .navigationDestination(for: Event.self) { EventDetailView(event: $0) }
                .searchable(text: $feed.query, prompt: "Search title and body")
                .onSubmit(of: .search) { Task { await feed.search(model) } }
                .task(id: feed.searchKey) {
                    // Debounce typing; a filter change searches at once too.
                    try? await Task.sleep(for: .milliseconds(350))
                    guard !Task.isCancelled else { return }
                    await feed.search(model)
                }
                .refreshable {
                    await model.refresh()
                    if feed.filtering { await feed.search(model) }
                }
                .toolbar { toolbar }
                .overlay { InboxEmptyView(feed: feed, isEmpty: events.isEmpty) }
        }
        .onChange(of: model.openEventID) { _, id in Task { await open(id) } }
        // A view deleted elsewhere leaves the filter.
        .onChange(of: model.views.map(\.id)) { _, ids in
            if let id = feed.viewID, !ids.contains(id) { feed.viewID = nil }
        }
        .task { await open(model.openEventID) }
    }

    private var list: some View {
        List {
            if let error = model.lastError {
                Label(error, systemImage: "exclamationmark.icloud")
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            }
            ForEach(events) { e in
                NavigationLink(value: e) {
                    EventRow(event: e, unread: model.isUnread(e), showProject: feed.project == nil)
                }
                .swipeActions(edge: .leading) {
                    if model.isUnread(e) {
                        Button {
                            Task { await model.markRead(e) }
                        } label: {
                            Label("Read up to here", systemImage: "envelope.open")
                        }
                        .tint(.blue)
                    }
                }
            }
            if feed.canLoadMore(model) {
                ProgressView()
                    .frame(maxWidth: .infinity)
                    .task { await feed.loadMore(model) }
            }
        }
        .listStyle(.plain)
    }

    @ToolbarContentBuilder
    private var toolbar: some ToolbarContent {
        ToolbarItem(placement: .primaryAction) {
            InboxFilterMenu(feed: feed)
        }
        ToolbarItem(placement: .secondaryAction) {
            Button {
                Task { await model.markAllRead() }
            } label: {
                Label("Mark all read", systemImage: "checkmark.circle")
            }
            .disabled(model.unread.ids.isEmpty && model.unread.badge == 0)
        }
    }

    /// Opens an event from a notification tap, fetching it when it is not cached yet.
    private func open(_ id: String?) async {
        guard let id else { return }
        model.openEventID = nil
        if let e = model.cache.events.first(where: { $0.id == id }) {
            path = [e]
        } else if let e = try? await model.client().event(id: id) {
            path = [e]
        }
    }
}

/// The Inbox's empty state: not set up, no match, loading, or no events.
struct InboxEmptyView: View {
    @Environment(AppModel.self) private var model
    let feed: InboxFeed
    let isEmpty: Bool

    var body: some View {
        if isEmpty {
            if !model.isConfigured {
                ContentUnavailableView("Not set up", systemImage: "gearshape",
                                       description: Text("Enter the server URL and tokens in Settings."))
            } else if feed.filtering {
                if feed.viewUnsupported {
                    ContentUnavailableView("Views not available", systemImage: SavedView.defaultSymbol,
                                           description: Text("The server does not support views."))
                } else if feed.searching {
                    ProgressView()
                } else {
                    #if os(macOS)
                    if feed.trimmedQuery.isEmpty {
                        ContentUnavailableView("No events", systemImage: "line.3.horizontal.decrease.circle",
                                               description: Text("No events match the filters."))
                    } else {
                        ContentUnavailableView.search(text: feed.trimmedQuery)
                    }
                    #else
                    ContentUnavailableView.search(text: feed.query)
                    #endif
                }
            } else if model.isSyncing {
                ProgressView()
            } else {
                ContentUnavailableView {
                    Label {
                        Text("No notifications")
                    } icon: {
                        Image("HeraldoMark").resizable().scaledToFit().frame(height: 64).accessibilityHidden(true)
                    }
                } description: {
                    Text("Events from the server appear here.")
                }
            }
        }
    }
}

struct EventRow: View {
    let event: Event
    var unread = false
    /// Shows the event's project as a chip. Off when a project filter is set.
    var showProject = true

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            KindIcon(event: event)
            VStack(alignment: .leading, spacing: 2) {
                HStack(alignment: .firstTextBaseline) {
                    Text(event.headline)
                        .font(.headline.weight(unread ? .semibold : .regular))
                        .lineLimit(2)
                    Spacer(minLength: 4)
                    if let d = event.date {
                        Text(d, format: .relative(presentation: .numeric, unitsStyle: .narrow))
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
                if let line = event.summary ?? event.body.map(Markdown.plainText), !line.isEmpty, line != event.headline {
                    Text(line)
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                        .lineLimit(2)
                }
                HStack(spacing: 6) {
                    Text([event.source, event.host].compactMap { $0 }.joined(separator: " · "))
                        .font(.caption)
                        .foregroundStyle(.tertiary)
                    if showProject, let project = event.project {
                        ProjectChip(project: project)
                    }
                    FlapBadge(event: event)
                }
                if let hold = event.hold, !hold.label.isEmpty {
                    Label(hold.label, systemImage: "tray.full")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                } else if event.wasHeldBack {
                    Label("Not pushed", systemImage: "bell.slash")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .padding(.vertical, 2)
        .overlay(alignment: .leading) {
            if unread {
                Circle().fill(.tint).frame(width: 8, height: 8).offset(x: -14)
                    .accessibilityLabel("Unread")
            }
        }
    }
}

/// A symbol for the event's kind, colored by level.
struct KindIcon: View {
    let event: Event

    var body: some View {
        Image(systemName: symbol)
            .font(.title3)
            .foregroundStyle(color)
            .frame(width: 28)
            .accessibilityHidden(true)
    }

    private var symbol: String {
        switch event.kind {
        case "incident":
            switch event.action {
            case "resolve": return "checkmark.seal"
            case "ack": return "hand.raised"
            default: return "exclamationmark.triangle"
            }
        case "digest": return "list.bullet.rectangle"
        case "checkin": return event.isFailingCheckin ? "clock.badge.xmark" : "clock.badge.checkmark"
        case "progress": return "gauge.with.dots.needle.33percent"
        case "status": return "chart.bar"
        default: return "bell"
        }
    }

    private var color: Color {
        if event.kind == "incident" && event.action == "resolve" { return .green }
        if event.isFailingCheckin { return .red }
        switch event.level {
        case "critical": return .red
        case "time-sensitive": return .orange
        case "passive": return .secondary
        default: return .accentColor
        }
    }
}
