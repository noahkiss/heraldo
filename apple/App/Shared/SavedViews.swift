import HeraldoKit
import SwiftUI

extension SavedView {
    static let defaultSymbol = "line.3.horizontal.decrease.circle"

    /// The view's icon when it names an SF Symbol, else the default.
    var symbol: String {
        guard let icon else { return Self.defaultSymbol }
        #if os(macOS)
        let known = NSImage(systemSymbolName: icon, accessibilityDescription: nil) != nil
        #else
        let known = UIImage(systemName: icon) != nil
        #endif
        return known ? icon : Self.defaultSymbol
    }
}

extension AppModel {
    /// Stores a new view with the write token.
    @discardableResult
    func saveView(name: String, filter: ViewFilter) async throws -> SavedView {
        let v = try await client().createView(ViewInput(name: name, filter: filter))
        await reloadViews()
        return v
    }

    /// Deletes a view with the write token.
    func deleteView(_ view: SavedView) async throws {
        try await client().deleteView(id: view.id)
        await reloadViews()
    }
}

extension View {
    /// Asks for a name, then stores the filter as a new view.
    func saveViewPrompt(isPresented: Binding<Bool>, filter: @escaping () -> ViewFilter) -> some View {
        modifier(SaveViewPrompt(isPresented: isPresented, filter: filter))
    }
}

private struct SaveViewPrompt: ViewModifier {
    @Environment(AppModel.self) private var model
    @Binding var isPresented: Bool
    let filter: () -> ViewFilter
    @State private var name = ""

    private var trimmed: String { name.trimmingCharacters(in: .whitespaces) }

    func body(content: Content) -> some View {
        content.alert("Save as View", isPresented: $isPresented) {
            TextField("Name", text: $name)
            Button("Save") {
                let f = filter()
                let n = String(trimmed.prefix(64))
                name = ""
                Task {
                    do {
                        try await model.saveView(name: n, filter: f)
                    } catch {
                        model.lastError = error.localizedDescription
                    }
                }
            }
            .disabled(trimmed.isEmpty)
            Button("Cancel", role: .cancel) { name = "" }
        }
    }
}
