import HeraldoKit
import SwiftUI

/// The middle column: the list of the home or project the sidebar chose.
struct MacContentList: View {
    @Environment(AppModel.self) private var model
    @Environment(\.openWindow) private var openWindow
    @Bindable var state: MacWindowState

    var body: some View {
        list
            .navigationTitle(title)
            .navigationSubtitle(subtitle)
            .contextMenu(forSelectionType: MacDetailItem.self) { items in
                if let item = items.first { MacItemMenu(state: state, item: item) }
            } primaryAction: { items in
                // Return or a double-click opens the item in its own window.
                for item in items { openWindow(id: MacAppDelegate.detailWindowID, value: item) }
            }
    }

    private var title: String {
        switch state.sidebar {
        case .home(let h): return h.title
        case .project(let p): return p
        case .view: return state.savedView?.name ?? "View"
        case nil: return AppInfo.displayName()
        }
    }

    private var subtitle: String {
        switch state.sidebar {
        case .home(.inbox):
            let n = model.unread.badge
            return n == 0 ? "" : n == 1 ? "1 unread" : "\(n) unread"
        case .home(.incidents):
            let n = model.unackedIncidents
            return n == 0 ? "" : n == 1 ? "1 open" : "\(n) open"
        default:
            return ""
        }
    }

    @ViewBuilder
    private var list: some View {
        switch state.sidebar {
        case .home(.inbox), nil: MacEventList(state: state, feed: state.inbox)
        case .home(.incidents): MacIncidentList(state: state)
        case .home(.checkins): MacCheckList(state: state)
        case .home(.activities): MacActivityList(state: state)
        case .home(.held): MacHeldList(state: state)
        case .project(let p): MacProjectList(state: state, project: p)
        case .view: MacEventList(state: state, feed: state.viewFeed)
        }
    }
}

/// The Inbox: the synced history, or server results while a search or
/// filter is set.
struct MacEventList: View {
    @Environment(AppModel.self) private var model
    @Bindable var state: MacWindowState
    let feed: InboxFeed

    var body: some View {
        let events = feed.events(model)
        List(selection: $state.selection) {
            ForEach(events) { e in
                MacEventRow(event: e, unread: model.isUnread(e))
                    .tag(MacDetailItem.event(e.id))
            }
            if feed.canLoadMore(model) {
                ProgressView()
                    .controlSize(.small)
                    .frame(maxWidth: .infinity)
                    .task { await feed.loadMore(model) }
            }
        }
        .overlay { InboxEmptyView(feed: feed, isEmpty: events.isEmpty) }
    }
}

/// Open incidents, acknowledged ones, stale ones, then the ones resolved
/// today.
struct MacIncidentList: View {
    @Environment(AppModel.self) private var model
    @Bindable var state: MacWindowState

    var body: some View {
        let open = model.openIncidents.filter(match)
        let acked = model.ackedIncidents.filter(match)
        let stale = model.staleIncidents.filter(match)
        let resolved = model.resolvedToday.filter(match)
        List(selection: $state.selection) {
            section("Open", open)
            section("Acknowledged", acked)
            section("Stale", stale)
            section("Resolved today", resolved)
        }
        .overlay {
            if open.isEmpty && acked.isEmpty && stale.isEmpty && resolved.isEmpty {
                if state.query.isEmpty {
                    ContentUnavailableView("No open incidents", systemImage: "checkmark.seal")
                } else {
                    ContentUnavailableView.search(text: state.query)
                }
            }
        }
    }

    private func match(_ e: Event) -> Bool {
        state.matches(e.headline, e.summary, e.source, e.host, e.key, e.project)
    }

    @ViewBuilder
    private func section(_ title: String, _ events: [Event]) -> some View {
        if !events.isEmpty {
            Section(title) {
                ForEach(events) { e in
                    MacEventRow(event: e, unread: model.isUnread(e), time: e.action == "resolve" ? .openFor : .opened)
                        .tag(MacDetailItem.event(e.id))
                }
            }
        }
    }
}

/// Every check, the ones that need a look first.
struct MacCheckList: View {
    @Environment(AppModel.self) private var model
    @Bindable var state: MacWindowState

    var body: some View {
        let checks = model.checks.filter { state.matches($0.key, $0.host, $0.project, $0.lastLogLine) }
        List(selection: $state.selection) {
            MacCheckSections(checks: checks)
        }
        .overlay {
            if checks.isEmpty {
                if state.query.isEmpty {
                    ContentUnavailableView("No check-ins", systemImage: "clock.badge.checkmark")
                } else {
                    ContentUnavailableView.search(text: state.query)
                }
            }
        }
    }
}

/// Checks in sections by state: down, late, running, then the rest.
struct MacCheckSections: View {
    let checks: [Check]
    var showProject = true

    private static let order: [(String, [Check.State])] = [
        ("Down", [.down]), ("Late", [.late]), ("Running", [.started]), ("Up", [.up]), ("Paused", [.paused]),
        ("New", [.new]),
    ]

    var body: some View {
        ForEach(Self.order, id: \.0) { title, states in
            let group = checks.filter { states.contains($0.state) }
            if !group.isEmpty {
                Section(title) {
                    ForEach(group) { c in
                        MacCheckRow(check: c, showProject: showProject)
                            .tag(MacDetailItem.check(c.key))
                    }
                }
            }
        }
    }
}

/// The Live Activities that are running.
struct MacActivityList: View {
    @Environment(AppModel.self) private var model
    @Bindable var state: MacWindowState

    var body: some View {
        let list = model.activities.filter {
            state.matches($0.headline, $0.key, $0.source, $0.content?.body, $0.content?.status)
        }
        List(selection: $state.selection) {
            ForEach(list) { a in
                MacActivityRow(activity: a)
                    .tag(MacDetailItem.activity(a.key))
            }
        }
        .overlay {
            if list.isEmpty {
                if state.query.isEmpty {
                    ContentUnavailableView("No Live Activities", systemImage: MacHome.activities.symbol,
                                           description: Text("Running jobs appear here."))
                } else {
                    ContentUnavailableView.search(text: state.query)
                }
            }
        }
    }
}

/// The alerts the hold keeps back until triage reads them.
struct MacHeldList: View {
    @Environment(AppModel.self) private var model
    @Bindable var state: MacWindowState

    var body: some View {
        let list = model.held.filter { state.matches($0.headline, $0.body, $0.source, $0.tag) }
        List(selection: $state.selection) {
            ForEach(list) { h in
                MacHeldRow(entry: h)
                    .tag(MacDetailItem.held(h.id))
            }
        }
        .overlay {
            if list.isEmpty {
                if state.query.isEmpty {
                    ContentUnavailableView("Nothing held", systemImage: MacHome.held.symbol)
                } else {
                    ContentUnavailableView.search(text: state.query)
                }
            }
        }
    }
}

/// One project: its current incidents, its checks, then its events.
struct MacProjectList: View {
    @Environment(AppModel.self) private var model
    @Bindable var state: MacWindowState
    let project: String

    var body: some View {
        let feed = state.projectFeed
        let incidents = model.incidents.filter {
            $0.project == project && state.matches($0.headline, $0.summary, $0.source, $0.host, $0.key)
        }
        let checks = model.checks.filter { $0.project == project && state.matches($0.key, $0.host, $0.lastLogLine) }
        let events = feed.events(model)
        List(selection: $state.selection) {
            if !incidents.isEmpty {
                Section("Incidents") {
                    ForEach(incidents) { e in
                        MacEventRow(event: e, unread: model.isUnread(e), showProject: false, time: .opened)
                            .tag(MacDetailItem.event(e.id))
                    }
                }
            }
            if !checks.isEmpty && !feed.hasMenuFilter {
                MacCheckSections(checks: checks, showProject: false)
            }
            Section("Events") {
                ForEach(events) { e in
                    MacEventRow(event: e, unread: model.isUnread(e), showProject: false)
                        .tag(MacDetailItem.event(e.id))
                }
                if feed.canLoadMore(model) {
                    ProgressView()
                        .controlSize(.small)
                        .frame(maxWidth: .infinity)
                        .task { await feed.loadMore(model) }
                }
            }
        }
        .overlay {
            if incidents.isEmpty && checks.isEmpty && events.isEmpty {
                if feed.searching {
                    ProgressView()
                } else if state.query.isEmpty {
                    ContentUnavailableView("No events", systemImage: "folder",
                                           description: Text("No events in \(project)."))
                } else {
                    ContentUnavailableView.search(text: state.query)
                }
            }
        }
    }
}

/// The context menu of a list item, and of the Event menu's commands.
struct MacItemMenu: View {
    @Environment(AppModel.self) private var model
    @Environment(\.openWindow) private var openWindow
    @Environment(\.openURL) private var openURL
    let state: MacWindowState
    let item: MacDetailItem

    var body: some View {
        Button("Open in New Window") { openWindow(id: MacAppDelegate.detailWindowID, value: item) }
        switch item {
        case .event(let id):
            if let e = state.event(id) {
                if let url = e.url.flatMap(URL.init(string:)) {
                    Button("Open Link") { openURL(url) }
                }
                Divider()
                if model.isUnread(e) {
                    Button("Mark Read up to Here") { Task { await model.markRead(e) } }
                }
                if e.isOpenIncident && !e.isAcked && model.canWrite {
                    Button("Acknowledge") { model.perform { try await $0.ack(e) } }
                    Button("Snooze 1 h") { model.perform { try await $0.snooze(e) } }
                }
                if let p = e.project, model.canWrite {
                    Button("Mute Project for 1 h") { model.perform { try await $0.muteProject(p) } }
                }
                Divider()
                Button("Copy ID") { copy(e.id) }
            }
        case .check(let key):
            if let c = model.checks.first(where: { $0.key == key }), model.canWrite {
                let pause = c.state != .paused
                Button(pause ? "Pause" : "Resume") { model.perform { try await $0.setPaused(c, pause) } }
            }
            Button("Copy Key") { copy(key) }
        case .activity(let key):
            Button("Copy Key") { copy(key) }
        case .held(let id):
            Button("Copy ID") { copy(id) }
        }
    }

    private func copy(_ s: String) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(s, forType: .string)
    }
}
