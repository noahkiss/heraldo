import HeraldoKit
import Observation
import SwiftUI

/// The homes in the Mac window's sidebar, in their ⌘1–⌘5 order.
enum MacHome: String, CaseIterable, Hashable, Codable {
    case inbox, incidents, checkins, activities, held

    var title: String {
        switch self {
        case .inbox: return "Inbox"
        case .incidents: return "Incidents"
        case .checkins: return "Check-ins"
        case .activities: return "Live Activities"
        case .held: return "Held"
        }
    }

    var symbol: String {
        switch self {
        case .inbox: return "tray"
        case .incidents: return "exclamationmark.triangle"
        case .checkins: return "clock.badge.checkmark"
        case .activities: return "gauge.with.dots.needle.33percent"
        case .held: return "tray.full"
        }
    }

    /// The ⌘ digit that selects it.
    var digit: Character {
        Character(String((MacHome.allCases.firstIndex(of: self) ?? 0) + 1))
    }
}

/// A sidebar row: a home, or one project.
enum MacSidebarItem: Hashable, Codable {
    case home(MacHome)
    case project(String)
}

/// What the detail column shows. Ids, not values, so a selection survives
/// a refresh that replaces the lists.
enum MacDetailItem: Hashable, Codable {
    case event(String)
    case check(String)
    case activity(String)
    case held(String)
}

/// One Mac window's state: the sidebar choice, the selection in each list,
/// the search text and the Inbox and project feeds.
@MainActor
@Observable
final class MacWindowState {
    let model: AppModel
    var sidebar: MacSidebarItem? = .home(.inbox) {
        didSet { if sidebar != oldValue { sidebarChanged() } }
    }
    /// The selection in each list, so switching back keeps it.
    private var selections: [MacSidebarItem: MacDetailItem] = [:]
    /// The search field's text. It applies to the list on screen.
    var query = "" {
        didSet { inbox.query = query; projectFeed.query = query }
    }
    let inbox = InboxFeed()
    /// The events of the project the sidebar has chosen.
    let projectFeed = InboxFeed()
    /// Events fetched by id, for a selection no list holds yet.
    private(set) var fetched: [String: Event] = [:]
    /// Bumped by ⌘F; the window focuses the search field.
    var searchFocusRequest = 0

    init(model: AppModel) {
        self.model = model
    }

    var selection: MacDetailItem? {
        get { sidebar.flatMap { selections[$0] } }
        set { if let sidebar { selections[sidebar] = newValue } }
    }

    var home: MacHome? {
        if case .home(let h) = sidebar { return h }
        return nil
    }

    var project: String? {
        if case .project(let p) = sidebar { return p }
        return nil
    }

    /// The feed of the list on screen, when it is an event list.
    var feed: InboxFeed? {
        switch sidebar {
        case .home(.inbox): return inbox
        case .project: return projectFeed
        default: return nil
        }
    }

    private func sidebarChanged() {
        query = ""
        projectFeed.project = project
        if project != nil { projectFeed.clearMenuFilters() }
    }

    func show(_ home: MacHome) { sidebar = .home(home) }

    /// Shows one event in the Inbox, from a notification or a link.
    func showEvent(_ id: String) async {
        sidebar = .home(.inbox)
        query = ""
        inbox.clearMenuFilters()
        selection = .event(id)
        if event(id) == nil, let e = try? await model.client().event(id: id) { fetched[id] = e }
    }

    /// The event with this id from any list the window holds.
    func event(_ id: String) -> Event? {
        if let e = fetched[id] { return e }
        for list in [inbox.events(model), projectFeed.events(model), model.incidents, model.resolvedToday,
                     model.cache.events] {
            if let e = list.first(where: { $0.id == id }) { return e }
        }
        return nil
    }

    func remember(_ e: Event) { fetched[e.id] = e }

    /// The selected event, when the selection is one.
    var selectedEvent: Event? {
        if case .event(let id) = selection { return event(id) }
        return nil
    }

    var selectedCheck: Check? {
        if case .check(let key) = selection { return model.checks.first { $0.key == key } }
        return nil
    }

    /// Syncs, then fetches the list on screen again.
    func refresh() async {
        await model.refresh()
        if let feed, feed.filtering { await feed.search(model) }
    }

    /// The search text matches a value: every word appears, ignoring case.
    func matches(_ fields: String?...) -> Bool {
        let words = query.split(separator: " ").map(String.init)
        guard !words.isEmpty else { return true }
        let text = fields.compactMap { $0 }.joined(separator: " ")
        return words.allSatisfy { text.localizedCaseInsensitiveContains($0) }
    }
}

extension FocusedValues {
    /// The key Mac window's state, for the menu commands.
    @Entry var macWindow: MacWindowState?
}
