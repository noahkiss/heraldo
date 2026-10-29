import HeraldoKit
import SwiftUI

/// The menu bar commands of the main window: search, refresh, the homes,
/// and the actions on the selected item.
struct MacCommands: Commands {
    @FocusedValue(\.macWindow) private var window
    @Environment(\.openWindow) private var openWindow
    @Environment(\.openURL) private var openURL

    var body: some Commands {
        CommandGroup(replacing: .textEditing) {
            Button("Find…") { window?.searchFocusRequest += 1 }
                .keyboardShortcut("f")
                .disabled(window == nil)
        }
        CommandGroup(after: .sidebar) {
            Button("Refresh") { Task { await window?.refresh() } }
                .keyboardShortcut("r")
                .disabled(window == nil || !(window?.model.isConfigured ?? false))
            Divider()
            ForEach(MacHome.allCases, id: \.self) { h in
                Button(h.title) { window?.show(h) }
                    .keyboardShortcut(KeyEquivalent(h.digit))
                    .disabled(window == nil || (h == .held && (window?.model.held.isEmpty ?? true)))
            }
            Divider()
        }
        CommandMenu("Event") {
            let model = window?.model
            let item = window?.selection
            let event = window?.selectedEvent
            let check = window?.selectedCheck
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
                if let event, let model { Task { await model.markRead(event) } }
            }
            .keyboardShortcut("u", modifiers: [.command, .shift])
            .disabled(event.map { !(model?.isUnread($0) ?? false) } ?? true)
            Button("Mark All Read") {
                if let model { Task { await model.markAllRead() } }
            }
            .keyboardShortcut("u", modifiers: [.command, .shift, .option])
            .disabled(model.map { !$0.isConfigured || $0.unread.badge == 0 } ?? true)
            Divider()
            Button("Acknowledge") {
                if let event, let model { Task { try? await model.ack(event) } }
            }
            .keyboardShortcut("a", modifiers: [.command, .shift])
            .disabled(!(model?.canWrite ?? false) || !(event.map { $0.isOpenIncident && !$0.isAcked } ?? false))
            Button(check?.state == .paused ? "Resume Check" : "Pause Check") {
                if let check, let model { Task { try? await model.setPaused(check, check.state != .paused) } }
            }
            .keyboardShortcut("p", modifiers: [.command, .shift])
            .disabled(!(model?.canWrite ?? false) || check == nil)
            Button("Mute Project for 1 h") {
                if let p = event?.project, let model { Task { try? await model.muteProject(p) } }
            }
            .disabled(!(model?.canWrite ?? false) || event?.project == nil)
        }
    }
}
