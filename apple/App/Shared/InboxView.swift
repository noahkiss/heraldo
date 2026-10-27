import HeraldoKit
import SwiftUI

/// The history: the synced cache, or server results while a search or filter is set.
struct InboxView: View {
    @Environment(AppModel.self) private var model
    @State private var path: [Event] = []
    @State private var query = ""
    @State private var kind: String?
    @State private var source: String?
    @State private var results: [Event]?
    @State private var resultsCursor: String?
    @State private var searching = false

    private var filtering: Bool {
        !query.trimmingCharacters(in: .whitespaces).isEmpty || kind != nil || source != nil
    }

    private var events: [Event] { filtering ? (results ?? []) : model.cache.events }

    var body: some View {
        NavigationStack(path: $path) {
            list
                .navigationTitle("Inbox")
                .navigationDestination(for: Event.self) { EventDetailView(event: $0) }
                .searchable(text: $query, prompt: "Search title and body")
                .onSubmit(of: .search) { Task { await search() } }
                .task(id: SearchKey(query: query, kind: kind, source: source)) {
                    // Debounce typing; a filter change searches at once too.
                    try? await Task.sleep(for: .milliseconds(350))
                    guard !Task.isCancelled else { return }
                    await search()
                }
                .refreshable {
                    await model.refresh()
                    if filtering { await search() }
                }
                .toolbar { toolbar }
                .overlay { empty }
        }
        .onChange(of: model.openEventID) { _, id in Task { await open(id) } }
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
                    EventRow(event: e, unread: model.isUnread(e))
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
            if canLoadMore {
                ProgressView()
                    .frame(maxWidth: .infinity)
                    .task { await loadMore() }
            }
        }
        .listStyle(.plain)
    }

    private var canLoadMore: Bool {
        if filtering { return resultsCursor != nil && (results?.count ?? 0) > 0 }
        return !model.cache.reachedEnd && model.cache.olderCursor != nil && !model.cache.events.isEmpty
    }

    @ToolbarContentBuilder
    private var toolbar: some ToolbarContent {
        ToolbarItem(placement: .primaryAction) {
            Menu {
                Picker("Kind", selection: $kind) {
                    Text("All kinds").tag(String?.none)
                    ForEach(Kind.allCases, id: \.self) { Text($0.rawValue.capitalized).tag(String?.some($0.rawValue)) }
                }
                Picker("Source", selection: $source) {
                    Text("All sources").tag(String?.none)
                    ForEach(model.cache.sources, id: \.self) { Text($0).tag(String?.some($0)) }
                }
            } label: {
                Label("Filter", systemImage: kind == nil && source == nil
                      ? "line.3.horizontal.decrease.circle" : "line.3.horizontal.decrease.circle.fill")
            }
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

    @ViewBuilder
    private var empty: some View {
        if events.isEmpty {
            if !model.isConfigured {
                ContentUnavailableView("Not set up", systemImage: "gearshape",
                                       description: Text("Enter the server URL and tokens in Settings."))
            } else if filtering {
                if searching { ProgressView() } else { ContentUnavailableView.search(text: query) }
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

    private func search() async {
        guard filtering, let sync = try? model.sync() else {
            results = nil
            resultsCursor = nil
            return
        }
        searching = true
        defer { searching = false }
        let q = query.trimmingCharacters(in: .whitespaces)
        do {
            let page = try await sync.search(q: q.isEmpty ? nil : q, kind: kind, source: source)
            results = page.events
            resultsCursor = page.events.count < sync.pageSize ? nil : page.nextCursor
        } catch {
            model.lastError = error.localizedDescription
        }
    }

    private func loadMore() async {
        if filtering {
            guard let sync = try? model.sync(), let before = resultsCursor else { return }
            let q = query.trimmingCharacters(in: .whitespaces)
            do {
                let page = try await sync.search(q: q.isEmpty ? nil : q, kind: kind, source: source, before: before)
                results = (results ?? []) + page.events
                resultsCursor = page.events.count < sync.pageSize ? nil : page.nextCursor
            } catch {
                resultsCursor = nil
            }
        } else {
            await model.loadOlder()
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

    private struct SearchKey: Equatable {
        var query: String
        var kind: String?
        var source: String?
    }
}

struct EventRow: View {
    let event: Event
    var unread = false

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
                Text([event.source, event.host].compactMap { $0 }.joined(separator: " · "))
                    .font(.caption)
                    .foregroundStyle(.tertiary)
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
        case "checkin": return "clock.badge.checkmark"
        case "progress": return "gauge.with.dots.needle.33percent"
        case "status": return "chart.bar"
        default: return "bell"
        }
    }

    private var color: Color {
        if event.kind == "incident" && event.action == "resolve" { return .green }
        switch event.level {
        case "critical": return .red
        case "time-sensitive": return .orange
        case "passive": return .secondary
        default: return .accentColor
        }
    }
}
