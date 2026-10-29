import HeraldoKit
import Observation
import SwiftUI
import WidgetKit

/// The display preferences that follow the person to every device, through
/// the iCloud key-value store (HeraldoKit `SyncedPrefs`). Views read and set
/// them here; a change from another device lands here too.
@MainActor
@Observable
final class DisplayPrefs {
    static let shared = DisplayPrefs()

    @ObservationIgnored let store: SyncedPrefs
    @ObservationIgnored private var observer: NSObjectProtocol?

    var groupBy: GroupBy { didSet { if groupBy != oldValue { store.groupBy = groupBy } } }
    var defaultView: String? { didSet { if defaultView != oldValue { store.defaultView = defaultView } } }
    var inboxFilter: InboxFilter { didSet { if inboxFilter != oldValue { store.inboxFilter = inboxFilter } } }
    var shareProject: String? { didSet { if shareProject != oldValue { store.shareProject = shareProject } } }

    init(store: SyncedPrefs = .live()) {
        self.store = store
        groupBy = store.groupBy
        defaultView = store.defaultView
        inboxFilter = store.inboxFilter
        shareProject = store.shareProject
        store.pushMirror()
        log.info("prefs: \(store.modeDescription, privacy: .public)")
        observer = store.observe { _ in
            Task { @MainActor in DisplayPrefs.shared.reload() }
        }
    }

    var syncs: Bool { store.syncs }

    /// Carries the share sheet's last project to iCloud and takes it here.
    func syncFromExtensions() {
        store.pushMirror()
        if shareProject != store.shareProject { shareProject = store.shareProject }
    }

    /// Takes the values iCloud brought. The Inbox's filter applies at the
    /// next launch, so a change on another device does not move this list.
    func reload() {
        groupBy = store.groupBy
        defaultView = store.defaultView
        shareProject = store.shareProject
        inboxFilter = store.inboxFilter
    }

    /// The homes the default view can name, before saved views.
    static let homes: [(id: String, title: String)] = [
        ("inbox", "Inbox"), ("incidents", "Incidents"), ("checkins", "Check-ins"),
    ]

    /// The default view's value for a saved view.
    static func tag(view id: String) -> String { "view:" + id }

    /// The saved view the app opens on, when the default view names one.
    var startViewID: String? {
        guard let v = defaultView, v.hasPrefix("view:") else { return nil }
        let id = String(v.dropFirst(5))
        return id.isEmpty ? nil : id
    }
}

extension InboxFeed {
    /// Starts from the saved Inbox filter. `withProject` is off where the
    /// list has no project filter, as in the Mac window's Inbox.
    func restore(_ f: InboxFilter, withProject: Bool = true) {
        kind = f.kind
        source = f.source
        hold = f.hold
        if withProject { project = f.project }
        showRoutine = f.showRoutine
    }

    /// Saves this feed's filters as the Inbox filter. Without `withProject`
    /// the saved project stays as it is.
    func saveFilter(_ prefs: DisplayPrefs, withProject: Bool = true) {
        prefs.inboxFilter = InboxFilter(kind: kind, source: source, hold: hold,
                                        project: withProject ? project : prefs.inboxFilter.project,
                                        showRoutine: showRoutine)
    }
}

/// The Display section of Settings: the prefs that sync through iCloud.
struct DisplaySection: View {
    @Environment(AppModel.self) private var model
    @Bindable var prefs = DisplayPrefs.shared

    var body: some View {
        Section {
            #if os(iOS)
            Picker("Group notifications", selection: $prefs.groupBy) {
                Text("By thread or source").tag(GroupBy.thread)
                Text("By project").tag(GroupBy.project)
            }
            #endif
            Picker("Open on", selection: $prefs.defaultView) {
                #if os(macOS)
                Text("Last used").tag(String?.none)
                #else
                Text("Inbox").tag(String?.none)
                #endif
                ForEach(DisplayPrefs.homes, id: \.id) { Text($0.title).tag(String?.some($0.id)) }
                if !model.views.isEmpty {
                    Section("Views") {
                        ForEach(model.views) { v in
                            Label(v.name, systemImage: v.symbol).tag(String?.some(DisplayPrefs.tag(view: v.id)))
                        }
                    }
                }
            }
            Picker("Share to project", selection: $prefs.shareProject) {
                Text("None").tag(String?.none)
                ForEach(model.projectChoices, id: \.self) { Text($0).tag(String?.some($0)) }
            }
        } header: {
            Text("Display")
        } footer: {
            Text(prefs.syncs ? "Synced with your other devices through iCloud."
                 : "Saved on this device only: this build has no iCloud key-value store.")
        }
    }
}

/// Tells the widgets that the app synced.
@MainActor
enum Widgets {
    /// The last snapshot written, with `updated` cleared.
    @MainActor private static var last: WidgetSnapshot?

    /// Writes the snapshot and reloads the widget kinds whose content
    /// changed. A reload wakes the widget extension. The baseline is the
    /// file, since the notification extension writes it too.
    @MainActor static func reload(_ model: AppModel) {
        DisplayPrefs.shared.syncFromExtensions()
        let snapshot = WidgetSnapshot(incidents: model.incidents, checks: model.checks,
                                      projects: model.projectChoices, events: model.cache.events)
        let kinds = snapshot.changedKinds(from: WidgetSnapshot.load() ?? last)
        guard !kinds.isEmpty else { return }
        do {
            try snapshot.save()
        } catch {
            log.error("widgets: \(error.localizedDescription, privacy: .public)")
        }
        last = snapshot
        for kind in kinds.sorted() { WidgetCenter.shared.reloadTimelines(ofKind: kind) }
    }
}
