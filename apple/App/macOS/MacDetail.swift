import HeraldoKit
import SwiftUI

/// The right column: the selected item, in a stack so History can push.
struct MacDetailColumn: View {
    let state: MacWindowState

    var body: some View {
        NavigationStack {
            Group {
                if let item = state.selection {
                    MacDetailView(item: item, state: state)
                } else {
                    ContentUnavailableView("Nothing selected", systemImage: "sidebar.squares.right")
                }
            }
            .navigationDestination(for: Event.self) { EventDetailView(event: $0) }
        }
        // A new selection starts a new stack, without the last one's History.
        .id(state.selection)
    }
}

/// A detail item in its own window: Return or a double-click in a list.
struct MacDetailWindow: View {
    @Environment(AppModel.self) private var model
    let item: MacDetailItem?

    var body: some View {
        NavigationStack {
            Group {
                if let item {
                    MacDetailView(item: item, state: nil)
                } else {
                    ContentUnavailableView("Nothing selected", systemImage: "sidebar.squares.right")
                }
            }
            .navigationDestination(for: Event.self) { EventDetailView(event: $0) }
        }
        .frame(minWidth: 420, minHeight: 360)
    }
}

/// One item of any home.
struct MacDetailView: View {
    @Environment(AppModel.self) private var model
    let item: MacDetailItem
    /// The window's state, to find an event its lists hold. Nil in a detail window.
    let state: MacWindowState?

    var body: some View {
        switch item {
        case .event(let id):
            if let e = state?.event(id) ?? model.cachedEvent(id) {
                EventDetailView(event: e)
            } else {
                EventByIDView(id: id)
            }
        case .check(let key):
            CheckDetailView(key: key)
        case .activity(let key):
            ActivityDetailView(key: key)
        case .held(let id):
            HeldDetailView(id: id)
        }
    }
}

extension AppModel {
    /// An event from the synced history or the current incidents.
    func cachedEvent(_ id: String) -> Event? {
        cache.events.first { $0.id == id } ?? incidents.first { $0.id == id }
    }
}

/// Fetches one event by id, then shows it.
struct EventByIDView: View {
    @Environment(AppModel.self) private var model
    let id: String
    @State private var event: Event?
    @State private var error: String?

    var body: some View {
        Group {
            if let event {
                EventDetailView(event: event)
            } else if let error {
                ContentUnavailableView("Event not found", systemImage: "questionmark.circle",
                                       description: Text(error))
            } else {
                ProgressView()
            }
        }
        .task(id: id) {
            do { event = try await model.client().event(id: id) } catch { self.error = error.localizedDescription }
        }
    }
}

/// One running activity: its content, its times and its history.
struct ActivityDetailView: View {
    @Environment(AppModel.self) private var model
    let key: String

    var body: some View {
        if let a = model.activities.first(where: { $0.key == key }) {
            content(a)
        } else {
            ContentUnavailableView("Activity ended", systemImage: MacHome.activities.symbol,
                                   description: Text("Its history keeps the progress."))
                .toolbar {
                    NavigationLink {
                        KeyHistoryView(key: key)
                    } label: {
                        Label("History", systemImage: "clock.arrow.circlepath")
                    }
                }
        }
    }

    private func content(_ a: ActivitySummary) -> some View {
        let c = a.content
        let state = c?.state
        let tint = activityTint(c?.tint)
        return ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                HStack(spacing: 10) {
                    Image(systemName: c?.symbol ?? MacHome.activities.symbol)
                        .font(.title)
                        .foregroundStyle(tint)
                        .accessibilityHidden(true)
                    Text(a.headline).font(.title2.bold()).textSelection(.enabled)
                }
                if let f = state?.fraction {
                    VStack(alignment: .leading, spacing: 4) {
                        ProgressView(value: f).tint(tint)
                        HStack {
                            if let t = state?.progressText { Text(t).monospacedDigit() }
                            Spacer()
                            if let trailing = c?.trailing { Text(trailing) }
                        }
                        .font(.callout)
                        .foregroundStyle(.secondary)
                    }
                    .accessibilityElement(children: .combine)
                }
                if let end = c?.endsAt, end > .now {
                    Label {
                        Text(timerInterval: Date.now...end, countsDown: true).monospacedDigit()
                    } icon: {
                        Image(systemName: "timer")
                    }
                    .font(.title3)
                }
                if let status = c?.status, !status.isEmpty {
                    Text(status).font(.body.weight(.medium))
                }
                if let body = c?.body, !body.isEmpty {
                    MarkdownView(text: body)
                }
                if let metrics = state?.shownMetrics, !metrics.isEmpty {
                    Grid(alignment: .leading, horizontalSpacing: 16, verticalSpacing: 6) {
                        ForEach(metrics, id: \.label) { m in
                            GridRow {
                                Text(m.label).foregroundStyle(.secondary)
                                Text(m.text).monospacedDigit().textSelection(.enabled)
                            }
                        }
                    }
                }
                NavigationLink {
                    KeyHistoryView(key: a.key)
                } label: {
                    Label("History", systemImage: "clock.arrow.circlepath")
                }
                .buttonStyle(.bordered)
                VStack(alignment: .leading, spacing: 6) {
                    Divider()
                    DetailFact("Source", a.source)
                    DetailFact("Key", a.key)
                    DetailFact("Started", a.startedAt?.formatted(date: .abbreviated, time: .shortened))
                    DetailFact("Updated", a.updatedAt?.formatted(date: .abbreviated, time: .shortened))
                    DetailFact("Devices", a.instances.map { list in
                        let active = list.filter { $0.state == "active" }.count
                        return active == 1 ? "1 showing it" : "\(active) showing it"
                    })
                }
                .font(.footnote)
            }
            .padding()
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .navigationTitle(a.source)
    }
}

/// One held alert: what the hold kept back, and when it forwards.
struct HeldDetailView: View {
    @Environment(AppModel.self) private var model
    let id: String

    var body: some View {
        if let h = model.held.first(where: { $0.id == id }) {
            ScrollView {
                VStack(alignment: .leading, spacing: 16) {
                    Text(h.headline).font(.title2.bold()).textSelection(.enabled)
                    if let body = h.body, !body.isEmpty, body != h.headline {
                        Text(body).textSelection(.enabled)
                    }
                    if let eventID = h.eventID {
                        NavigationLink {
                            EventByIDView(id: eventID)
                        } label: {
                            Label("Show Event", systemImage: "doc.text")
                        }
                        .buttonStyle(.bordered)
                    }
                    VStack(alignment: .leading, spacing: 6) {
                        Divider()
                        DetailFact("Source", h.source)
                        DetailFact("Client", h.client)
                        DetailFact("Tag", h.tag)
                        DetailFact("Held", h.date.formatted(date: .abbreviated, time: .shortened))
                        DetailFact("Forwards", h.deadline?.formatted(date: .abbreviated, time: .shortened))
                        DetailFact("Shadow", h.shadow == true ? "Yes" : nil)
                        DetailFact("ID", h.id)
                    }
                    .font(.footnote)
                }
                .padding()
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .navigationTitle("Held")
        } else {
            ContentUnavailableView("No longer held", systemImage: MacHome.held.symbol)
        }
    }
}

/// A name and its value, in a detail's facts.
struct DetailFact: View {
    let name: String
    let value: String?

    init(_ name: String, _ value: String?) {
        self.name = name
        self.value = value
    }

    var body: some View {
        if let value, !value.isEmpty {
            HStack(alignment: .firstTextBaseline) {
                Text(name).foregroundStyle(.secondary).frame(width: 72, alignment: .leading)
                Text(value).textSelection(.enabled)
            }
        }
    }
}
