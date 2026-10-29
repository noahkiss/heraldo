import HeraldoKit
import SwiftUI

/// The menu bar commands of the main window: search, refresh, the homes,
/// and the actions on the selected item.
struct MacCommands: Commands {
    @Environment(\.openWindow) private var openWindow
    @Environment(\.openURL) private var openURL

    private var window: MacWindowState { .shared }
    private var model: AppModel { window.model }

    var body: some Commands {
        CommandGroup(replacing: .textEditing) {
            Button("Find…") {
                model.showMain?()
                window.searchFocusRequest += 1
            }
            .keyboardShortcut("f")
        }
        CommandGroup(after: .sidebar) {
            Button("Refresh") { Task { await window.refresh() } }
                .keyboardShortcut("r")
                .disabled(!model.isConfigured)
            Divider()
            ForEach(MacHome.allCases, id: \.self) { h in
                Button(h.title) {
                    window.show(h)
                    model.showMain?()
                }
                .keyboardShortcut(KeyEquivalent(h.digit))
                .disabled(h == .held && model.held.isEmpty)
            }
            Divider()
        }
        CommandMenu("Event") {
            let item = window.selection
            let event = window.selectedEvent
            let check = window.selectedCheck
            Button("Open in New Window") {
                if let item { openWindow(id: MacAppDelegate.detailWindowID, value: item) }
            }
            .keyboardShortcut("o")
            .disabled(item == nil)
            Button("Open Link") {
                if let url = event?.url.flatMap(URL.init(string:)) { openURL(url) }
            }
            .keyboardShortcut("l", modifiers: [.command, .shift])
            .disabled(event?.url.flatMap(URL.init(string:)) == nil)
            Divider()
            Button("Mark Read up to Here") {
                if let event { Task { await model.markRead(event) } }
            }
            .keyboardShortcut("u", modifiers: [.command, .shift])
            .disabled(!(event.map(model.isUnread) ?? false))
            Button("Mark All Read") { Task { await model.markAllRead() } }
                .keyboardShortcut("u", modifiers: [.command, .shift, .option])
                .disabled(!model.isConfigured || model.unread.inboxCount == 0)
            Divider()
            Button("Acknowledge") {
                if let event { model.perform { try await $0.ack(event) } }
            }
            .keyboardShortcut("a", modifiers: [.command, .shift])
            .disabled(!model.canWrite || !(event.map { $0.isOpenIncident && !$0.isAcked } ?? false))
            Button(check?.state == .paused ? "Resume Check" : "Pause Check") {
                if let check { model.perform { try await $0.setPaused(check, check.state != .paused) } }
            }
            .keyboardShortcut("p", modifiers: [.command, .shift])
            .disabled(!model.canWrite || check == nil)
            Button("Mute Project for 1 h") {
                if let p = event?.project { model.perform { try await $0.muteProject(p) } }
            }
            .disabled(!model.canWrite || event?.project == nil)
        }
    }
}

extension AppModel {
    /// Runs a write; a failure shows as the last error.
    func perform(_ action: @escaping @MainActor (AppModel) async throws -> Void) {
        Task {
            do { try await action(self) } catch { lastError = error.localizedDescription }
        }
    }
}
