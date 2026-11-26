import HeraldoKit
import SwiftUI

/// Every source the server has stored an event from, new ones first, with
/// how many of its last 30 days' events counted toward the badge. A row for
/// a source starts from here, so the badge is the owner's to set.
struct SourcesView: View {
    @Environment(AppModel.self) private var model
    @Bindable var store: DeliveryStore
    @State private var show: Show = .all
    @State private var query = ""
    @State private var adding: EditTarget?

    enum Show: String, CaseIterable, Identifiable {
        case all = "All"
        case badge = "In the badge"
        case new = "New"
        var id: String { rawValue }
    }

    private var canEdit: Bool { !(model.credentials.adminToken ?? "").isEmpty }

    private var shown: [Source] {
        let q = query.trimmingCharacters(in: .whitespaces).lowercased()
        return store.sources.filter { s in
            switch show {
            case .all: break
            case .badge: if s.badgeIn == 0 { return false }
            case .new: if !s.new { return false }
            }
            return q.isEmpty || s.source.contains(q)
        }
    }

    var body: some View {
        Form {
            Section {
                Picker("Show", selection: $show) {
                    ForEach(Show.allCases) { Text($0.rawValue).tag($0) }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
            } footer: {
                Text("Counts cover the last 30 days, routine events left out. In the badge: events that counted toward it while unread. Add a row to change what a source does.")
            }

            Section {
                if shown.isEmpty {
                    Text(store.loaded ? "No sources" : "Loading").foregroundStyle(.secondary)
                }
                ForEach(shown) { s in
                    SourceRow(source: s)
                        .contextMenu { menu(s) }
                        .swipeActions {
                            if s.new && canEdit {
                                Button("Reviewed") { review(s) }.tint(.blue)
                            }
                            if canEdit {
                                Button("Add Row") { addRow(s) }
                            }
                        }
                }
            }

            if let error = store.error {
                Section { Text(error).font(.footnote).foregroundStyle(.red) }
            }
        }
        .formStyle(.grouped)
        .navigationTitle("Sources")
        .searchable(text: $query, prompt: "Search sources")
        .refreshable { await store.load(model) }
        .task { await store.load(model) }
        .sheet(item: $adding) { DeliveryRowEditor(store: store, target: $0) }
    }

    @ViewBuilder
    private func menu(_ s: Source) -> some View {
        Button("Mark Reviewed") { review(s) }
            .disabled(!canEdit || !s.new)
        Button("Add Row for \(s.source)…") { addRow(s) }
            .disabled(!canEdit)
    }

    private func review(_ s: Source) {
        Task { await store.write(model) { try await $0.reviewSource(s.source) } }
    }

    private func addRow(_ s: Source) {
        adding = .new(DeliverySettingInput(match: DeliveryMatch(source: s.source), position: store.nextPosition))
    }
}

/// One source: its name, whether it is new, and its badge counts.
struct SourceRow: View {
    let source: Source

    var body: some View {
        HStack(alignment: .firstTextBaseline) {
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 6) {
                    Text(source.source).lineLimit(1)
                    if source.new {
                        Text("New")
                            .font(.caption2.weight(.semibold))
                            .padding(.horizontal, 5)
                            .padding(.vertical, 1)
                            .background(.tint.opacity(0.15), in: .capsule)
                            .foregroundStyle(.tint)
                    }
                }
                Text(detail).font(.caption).foregroundStyle(.secondary)
            }
            Spacer(minLength: 8)
            Label("\(source.badgeIn)", systemImage: source.badgeIn > 0 ? "app.badge.fill" : "app.badge")
                .monospacedDigit()
                .foregroundStyle(source.badgeIn > 0 ? AnyShapeStyle(.tint) : AnyShapeStyle(.secondary))
                .help("Events that counted toward the badge")
        }
        .accessibilityElement(children: .combine)
        .accessibilityValue(source.new ? "new, \(source.badgeIn) in the badge" : "\(source.badgeIn) in the badge")
    }

    private var detail: String {
        var parts = [source.events == 1 ? "1 event" : "\(source.events) events",
                     "\(source.badgeIn) in the badge, \(source.badgeOut) not"]
        if let last = source.lastSeen {
            parts.append("last " + last.formatted(.relative(presentation: .named)))
        }
        return parts.joined(separator: " · ")
    }
}
