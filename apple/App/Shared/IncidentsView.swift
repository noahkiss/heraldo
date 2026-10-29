import HeraldoKit
import SwiftUI

/// Incidents: open ones first, then acknowledged ones, from `/v1/state`; then
/// the ones resolved in the last day, from the synced history. With more
/// than one project present, each project gets its own section, in the same
/// order inside it.
struct IncidentsView: View {
    @Environment(AppModel.self) private var model
    @State private var error: String?

    private var open: [Event] { model.incidents.filter { !$0.isAcked } }
    private var acked: [Event] { model.incidents.filter(\.isAcked) }
    private var resolved: [Event] {
        let current = Set(model.incidents.compactMap(\.key))
        return model.cache.resolvedIncidents(since: .now.addingTimeInterval(-86_400))
            .filter { !current.contains($0.key ?? "") }
    }

    var body: some View {
        NavigationStack {
            List {
                if let error {
                    Text(error).font(.footnote).foregroundStyle(.red)
                }
                if let groups = ProjectGroup.split(open + acked + resolved, by: \.project) {
                    ForEach(groups, id: \.title) { g in
                        Section(g.title) {
                            ForEach(g.items) { row($0) }
                        }
                    }
                } else {
                    section("Open", open)
                    section("Acknowledged", acked)
                    section("Resolved today", resolved)
                }
            }
            .listStyle(.plain)
            .navigationTitle("Incidents")
            .navigationDestination(for: Event.self) { EventDetailView(event: $0) }
            .refreshable { await model.refresh() }
            .overlay {
                if model.incidents.isEmpty && resolved.isEmpty {
                    ContentUnavailableView("No open incidents", systemImage: "checkmark.seal")
                }
            }
        }
    }

    @ViewBuilder
    private func section(_ title: String, _ events: [Event]) -> some View {
        if !events.isEmpty {
            Section(title) {
                ForEach(events) { row($0) }
            }
        }
    }

    private func row(_ e: Event) -> some View {
        NavigationLink(value: e) {
            HStack {
                EventRow(event: e, showProject: false)
                trailing(e)
            }
        }
        .swipeActions {
            if e.action != "resolve" && !e.isAcked && model.canWrite {
                Button {
                    Task {
                        do { try await model.ack(e); error = nil } catch { self.error = error.localizedDescription }
                    }
                } label: {
                    Label("Acknowledge", systemImage: "hand.raised")
                }
                .tint(.orange)
            }
        }
        .contextMenu {
            if let project = e.project, model.canWrite {
                Button {
                    Task {
                        do { try await model.muteProject(project); error = nil } catch { self.error = error.localizedDescription }
                    }
                } label: {
                    Label("Mute project for 1 h", systemImage: "bell.slash")
                }
            }
        }
    }

    /// When it opened, for a current incident; how long it was open, for a
    /// resolved one.
    @ViewBuilder
    private func trailing(_ e: Event) -> some View {
        if e.action == "resolve" {
            if let d = e.incident?.openFor {
                Text("after \(Duration.seconds(d).formatted(.units(allowed: [.days, .hours, .minutes], width: .narrow, maximumUnitCount: 2)))")
                    .font(.caption).foregroundStyle(.secondary)
            }
        } else if let opened = e.incident?.openedAt {
            Text(opened, format: .relative(presentation: .numeric, unitsStyle: .narrow))
                .font(.caption).foregroundStyle(.secondary)
                .accessibilityLabel(Text("Opened \(opened, format: .relative(presentation: .named))"))
        }
    }
}
