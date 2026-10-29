import HeraldoKit
import SwiftUI

/// The sidebar's Views section: one row per saved view, under Projects.
struct MacViewsSection: View {
    @Environment(AppModel.self) private var model
    let state: MacWindowState

    var body: some View {
        if model.viewsSupported && !model.views.isEmpty {
            Section("Views") {
                ForEach(model.views) { v in
                    Label(v.name, systemImage: v.symbol)
                        .tag(MacSidebarItem.view(v.id))
                        .contextMenu {
                            Button("Delete", role: .destructive) { delete(v) }
                                .disabled(!model.canWrite)
                        }
                }
            }
        }
    }

    private func delete(_ v: SavedView) {
        if state.viewID == v.id { state.show(.inbox) }
        model.perform { try await $0.deleteView(v) }
    }
}

extension MacWindowState {
    /// The view the sidebar has chosen.
    var viewID: String? {
        if case .view(let id) = sidebar { return id }
        return nil
    }

    var savedView: SavedView? {
        viewID.flatMap { id in model.views.first { $0.id == id } }
    }
}
