import HeraldoKit
import SwiftUI

/// The Mac window: the homes and projects in the sidebar, a list in the
/// middle and the selected item on the right.
struct MacRootView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.openSettings) private var openSettings
    @State private var state = MacWindowState.shared
    @State private var columns = NavigationSplitViewVisibility.all
    @FocusState private var searchFocused: Bool
    @AppStorage("mac.home") private var savedHome = MacHome.inbox.rawValue

    var body: some View {
        NavigationSplitView(columnVisibility: $columns) {
            // AppKit saves the column widths with the window.
            MacSidebar(state: state)
                .navigationSplitViewColumnWidth(min: 180, ideal: 220, max: 320)
        } content: {
            MacContentList(state: state)
                .navigationSplitViewColumnWidth(min: 320, ideal: 400, max: 640)
        } detail: {
            MacDetailColumn(state: state)
        }
        .searchable(text: $state.query, placement: .toolbar, prompt: searchPrompt)
        .searchFocused($searchFocused)
        .onSubmit(of: .search) { Task { await state.feed?.search(model) } }
        .toolbar { toolbar }
        .frame(minWidth: 820, minHeight: 480)
        .heraldoLinks()
        .task(id: state.feed?.searchKey) {
            // Debounce typing; a filter change searches at once too.
            try? await Task.sleep(for: .milliseconds(350))
            guard !Task.isCancelled else { return }
            await state.feed?.search(model)
        }
        .onChange(of: state.searchFocusRequest) { searchFocused = true }
        .onChange(of: model.openEventID) { _, id in open(id) }
        .onChange(of: state.home) { _, h in if let h { savedHome = h.rawValue } }
        // The Held row goes when the hold empties; its list goes with it.
        .onChange(of: model.held.isEmpty) { _, empty in
            if empty && state.home == .held { state.show(.inbox) }
        }
        .onAppear {
            if let h = MacHome(rawValue: savedHome), h != .held { state.show(h) }
            open(model.openEventID)
            // A provisioning launch saves the settings itself.
            if !model.isConfigured && !Provision.requested { openSettings() }
        }
    }

    private var searchPrompt: String {
        switch state.sidebar {
        case .home(.incidents): return "Search incidents"
        case .home(.checkins): return "Search check-ins"
        case .home(.activities): return "Search activities"
        case .home(.held): return "Search held alerts"
        case .project(let p): return "Search \(p)"
        default: return "Search title and body"
        }
    }

    @ToolbarContentBuilder
    private var toolbar: some ToolbarContent {
        ToolbarItemGroup(placement: .primaryAction) {
            if let feed = state.feed {
                InboxFilterMenu(feed: feed, showsProject: false)
                    .help("Filter")
            }
            Button {
                Task { await state.refresh() }
            } label: {
                Label("Refresh", systemImage: "arrow.clockwise")
            }
            .help("Refresh")
            .disabled(!model.isConfigured || model.isSyncing)
        }
    }

    private func open(_ id: String?) {
        guard let id else { return }
        model.openEventID = nil
        Task { await state.showEvent(id) }
    }
}

/// The sidebar: the homes with their counts, then one row per project.
struct MacSidebar: View {
    @Environment(AppModel.self) private var model
    @Bindable var state: MacWindowState

    var body: some View {
        List(selection: $state.sidebar) {
            Section {
                ForEach(homes, id: \.self) { h in
                    Label(h.title, systemImage: h.symbol)
                        .badge(count(h))
                        .tag(MacSidebarItem.home(h))
                        .accessibilityLabel(accessibility(h))
                }
            }
            if !model.projects.isEmpty {
                Section("Projects") {
                    ForEach(model.projects, id: \.self) { p in
                        projectRow(p)
                    }
                }
            }
        }
        .listStyle(.sidebar)
        .overlay(alignment: .bottom) { status }
    }

    /// Held shows only while the hold has entries, or while it is open.
    private var homes: [MacHome] {
        MacHome.allCases.filter { $0 != .held || !model.held.isEmpty || state.home == .held }
    }

    private func count(_ h: MacHome) -> Int {
        switch h {
        case .inbox: return model.unread.badge
        case .incidents: return model.unackedIncidents
        case .checkins: return model.checks.filter { $0.state == .down || $0.state == .late }.count
        case .activities: return model.activities.count
        case .held: return model.held.count
        }
    }

    private func accessibility(_ h: MacHome) -> Text {
        let n = count(h)
        guard n > 0 else { return Text(h.title) }
        switch h {
        case .inbox: return Text("\(h.title), \(n) unread")
        case .incidents: return Text("\(h.title), \(n) open")
        case .checkins: return Text("\(h.title), \(n) down or late")
        case .activities: return Text("\(h.title), \(n) running")
        case .held: return Text("\(h.title), \(n) held")
        }
    }

    private func projectRow(_ p: String) -> some View {
        let open = model.unackedIncidents(in: p)
        let down = model.checks.filter { $0.project == p && $0.state == .down }.count
        let attention = open + down > 0
        let unread = model.unreadCount(in: p)
        var parts = [p]
        if unread > 0 { parts.append("\(unread) unread") }
        if open > 0 { parts.append(open == 1 ? "1 open incident" : "\(open) open incidents") }
        if down > 0 { parts.append(down == 1 ? "1 check down" : "\(down) checks down") }
        return Label {
            Text(p)
        } icon: {
            Image(systemName: attention ? "exclamationmark.circle.fill" : "folder")
                .foregroundStyle(attention ? AnyShapeStyle(.red) : AnyShapeStyle(.secondary))
        }
        .badge(unread)
        .tag(MacSidebarItem.project(p))
        .accessibilityLabel(Text(parts.joined(separator: ", ")))
    }

    /// The last sync, or the error, under the list.
    @ViewBuilder
    private var status: some View {
        if let error = model.lastError {
            Label(error, systemImage: "exclamationmark.icloud")
                .font(.caption)
                .foregroundStyle(.secondary)
                .lineLimit(2)
                .padding(10)
                .frame(maxWidth: .infinity, alignment: .leading)
        } else if let at = model.lastSync {
            TimelineView(.periodic(from: .now, by: 30)) { _ in
                Text("Updated \(at, format: .relative(presentation: .named))")
                    .font(.caption)
                    .monospacedDigit()
                    .foregroundStyle(.tertiary)
            }
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}

extension AppModel {
    /// The unread events of one project in the synced history, routine
    /// events left out.
    func unreadCount(in project: String) -> Int {
        guard !unread.ids.isEmpty else { return 0 }
        return cache.events.filter { $0.project == project && !$0.isRoutine && unread.ids.contains($0.id) }.count
    }

    func unackedIncidents(in project: String) -> Int {
        incidents.filter { $0.project == project && !$0.isAcked }.count
    }
}
