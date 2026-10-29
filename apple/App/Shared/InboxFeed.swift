import HeraldoKit
import Observation
import SwiftUI

/// The Inbox's filters and the list they give: the synced cache, or server
/// results while a search or filter is set. The iOS Inbox tab and the Mac
/// window's Inbox and project lists each hold one.
@MainActor
@Observable
final class InboxFeed {
    var query = ""
    var kind: String?
    var source: String?
    /// A hold state, such as held: the triage filter.
    var hold: String?
    /// A project, or `noProject` for the events with none.
    var project: String?
    /// Routine events (passing check-ins, progress, status) stay out unless
    /// this is on, or the kind filter names their kind.
    var showRoutine = false
    /// A saved view's id: the server applies its filter, and the filters
    /// set here override the view's.
    var viewID: String?
    /// The server ignored the view: it is older than views.
    private(set) var viewUnsupported = false
    private(set) var results: [Event]?
    private(set) var resultsCursor: String?
    private(set) var searching = false

    var trimmedQuery: String { query.trimmingCharacters(in: .whitespaces) }

    var filtering: Bool {
        !trimmedQuery.isEmpty || kind != nil || source != nil || hold != nil || project != nil || viewID != nil
    }

    /// Whether a filter other than the project is set.
    var hasMenuFilter: Bool { kind != nil || source != nil || hold != nil || showRoutine }

    /// The routine filter for a server search: routine events stay out
    /// unless they are asked for, by the toggle or by the kind.
    /// A view keeps its own routine filter.
    var routineFilter: EventQuery.Routine? {
        if viewID != nil { return nil }
        return showRoutine || Event.isRoutineKind(kind) ? nil : .exclude
    }

    /// Routine events stay out of the plain list: a check's pings live in
    /// its history on the Check-ins tab, and a Live Activity shows progress.
    func events(_ model: AppModel) -> [Event] {
        let list = filtering ? (results ?? []) : model.cache.events
        return routineFilter == nil ? list : list.filter { !$0.isRoutine }
    }

    func canLoadMore(_ model: AppModel) -> Bool {
        if filtering { return resultsCursor != nil && (results?.count ?? 0) > 0 }
        return !model.cache.reachedEnd && model.cache.olderCursor != nil && !model.cache.events.isEmpty
    }

    /// Changes whenever the list must be fetched again: the debounce key.
    var searchKey: SearchKey {
        SearchKey(query: query, kind: kind, source: source, hold: hold, project: project, showRoutine: showRoutine,
                  viewID: viewID)
    }

    func clearMenuFilters() {
        kind = nil
        source = nil
        hold = nil
        showRoutine = false
    }

    func search(_ model: AppModel) async {
        guard filtering, let sync = try? model.sync() else {
            results = nil
            resultsCursor = nil
            return
        }
        searching = true
        defer { searching = false }
        let q = trimmedQuery
        do {
            let page = try await sync.search(q: q.isEmpty ? nil : q, kind: kind, source: source, hold: hold,
                                             project: project, routine: routineFilter, view: viewID)
            results = page.events
            resultsCursor = page.events.count < sync.pageSize ? nil : page.nextCursor
            viewUnsupported = false
        } catch {
            // A view's list never falls back to another list.
            if viewID != nil {
                results = []
                resultsCursor = nil
                viewUnsupported = (error as? HeraldoError) == .unsupported("views")
            }
            model.lastError = error.localizedDescription
        }
    }

    func loadMore(_ model: AppModel) async {
        if filtering {
            guard let sync = try? model.sync(), let before = resultsCursor else { return }
            let q = trimmedQuery
            do {
                let page = try await sync.search(q: q.isEmpty ? nil : q, kind: kind, source: source, hold: hold,
                                                 project: project, routine: routineFilter, view: viewID,
                                                 before: before)
                results = (results ?? []) + page.events
                resultsCursor = page.events.count < sync.pageSize ? nil : page.nextCursor
            } catch {
                resultsCursor = nil
            }
        } else {
            await model.loadOlder()
        }
    }

    struct SearchKey: Equatable {
        var query: String
        var kind: String?
        var source: String?
        var hold: String?
        var project: String?
        var showRoutine: Bool
        var viewID: String?
    }

    /// The filter set here, over the chosen view's: the filter "Save as
    /// View" stores. The hold filter is not part of a view.
    func viewFilter(_ model: AppModel) -> ViewFilter {
        var f = model.views.first { $0.id == viewID }?.filter ?? ViewFilter()
        if let project { f.project = [project] }
        if let kind { f.kind = kind }
        if let source { f.source = source }
        if !trimmedQuery.isEmpty { f.q = trimmedQuery }
        if viewID == nil { f.routine = routineFilter }
        return f
    }
}

/// The Inbox's filter menu: kind, source, project, hold and routine events.
struct InboxFilterMenu: View {
    @Environment(AppModel.self) private var model
    @Bindable var feed: InboxFeed
    /// Off where the project is already chosen, as in a Mac project list.
    var showsProject = true
    /// Off where the sidebar chooses the view, as on the Mac.
    var showsViews = true
    @State private var naming = false

    /// The filled icon: a filter is set.
    private var filled: Bool {
        feed.hasMenuFilter || (showsProject && feed.project != nil) || (showsViews && feed.viewID != nil)
    }

    var body: some View {
        Menu {
            if showsViews && !model.views.isEmpty {
                Picker("View", selection: $feed.viewID) {
                    Text("No view").tag(String?.none)
                    ForEach(model.views) { v in
                        Label(v.name, systemImage: v.symbol).tag(String?.some(v.id))
                    }
                }
            }
            Picker("Kind", selection: $feed.kind) {
                Text("All kinds").tag(String?.none)
                ForEach(Kind.allCases, id: \.self) { Text($0.rawValue.capitalized).tag(String?.some($0.rawValue)) }
            }
            Picker("Source", selection: $feed.source) {
                Text("All sources").tag(String?.none)
                ForEach(model.cache.sources, id: \.self) { Text($0).tag(String?.some($0)) }
            }
            if showsProject && !model.projectChoices.isEmpty {
                Picker("Project", selection: $feed.project) {
                    Text("All projects").tag(String?.none)
                    ForEach(model.projectChoices, id: \.self) { Text($0).tag(String?.some($0)) }
                    Text("No project").tag(String?.some(noProject))
                }
            }
            Picker("Hold", selection: $feed.hold) {
                Text("Any hold").tag(String?.none)
                ForEach(model.cache.holdStates, id: \.self) { Text($0.capitalized).tag(String?.some($0)) }
            }
            Toggle("Show routine events", isOn: $feed.showRoutine)
            if model.viewsSupported {
                Divider()
                Button("Save as View…") { naming = true }
                    .disabled(!model.canWrite)
            }
            #if os(macOS)
            Divider()
            Button("Clear filters") { feed.clearMenuFilters() }
                .disabled(!feed.hasMenuFilter)
            #endif
        } label: {
            Label("Filter", systemImage: filled ? "line.3.horizontal.decrease.circle.fill"
                  : "line.3.horizontal.decrease.circle")
        }
        .saveViewPrompt(isPresented: $naming) { feed.viewFilter(model) }
    }
}
