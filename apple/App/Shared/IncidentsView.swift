import HeraldoKit
import SwiftUI

/// Open incidents: the latest event of every incident key not resolved.
struct IncidentsView: View {
    @Environment(AppModel.self) private var model
    @State private var error: String?

    var body: some View {
        NavigationStack {
            List {
                if let error {
                    Text(error).font(.footnote).foregroundStyle(.red)
                }
                ForEach(model.incidents) { e in
                    NavigationLink(value: e) {
                        HStack {
                            EventRow(event: e)
                            if e.action == "ack" {
                                Text("Acked").font(.caption).foregroundStyle(.secondary)
                            }
                        }
                    }
                    .swipeActions {
                        if e.action != "ack" && !(model.credentials.writeToken?.isEmpty ?? true) {
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
                }
            }
            .listStyle(.plain)
            .navigationTitle("Incidents")
            .navigationDestination(for: Event.self) { EventDetailView(event: $0) }
            .refreshable { await model.refresh() }
            .overlay {
                if model.incidents.isEmpty {
                    ContentUnavailableView("No open incidents", systemImage: "checkmark.seal")
                }
            }
        }
    }
}
