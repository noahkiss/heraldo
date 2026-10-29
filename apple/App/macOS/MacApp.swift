import AppKit
import HeraldoKit
import SwiftUI
import UserNotifications

/// The Mac app's delegate: push, notification responses, heraldo:// links,
/// and the sync that keeps the menu bar current while no window is open.
final class MacAppDelegate: NSObject, NSApplicationDelegate, UNUserNotificationCenterDelegate {
    static let mainWindowID = "main"
    /// How often the menu bar syncs. The sync is a cursor read, so it is cheap.
    static let pollInterval: Duration = .seconds(60)

    private var poll: Task<Void, Never>?

    func applicationDidFinishLaunching(_ notification: Notification) {
        UNUserNotificationCenter.current().delegate = self
        let push = PushManager.shared
        AppModel.shared.onBadge = { push.setBadge($0) }
        AppModel.shared.onCredentialsChanged = { push.credentialsChanged() }
        push.start()
        if Provision.requested { Task { await Provision.run(AppModel.shared) } }
        startPolling()
        DockPolicy.start()
        // After sleep the network can take a few seconds to return; sync then.
        NSWorkspace.shared.notificationCenter.addObserver(
            forName: NSWorkspace.didWakeNotification, object: nil, queue: .main
        ) { _ in
            Task { @MainActor in
                try? await Task.sleep(for: .seconds(5))
                await AppModel.shared.refresh()
                await PushManager.shared.foreground()
            }
        }
    }

    /// The menu bar item stays when the window closes. DockPolicy hides the Dock icon.
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        if !flag { AppModel.shared.showMain?() }
        return true
    }

    func applicationDidBecomeActive(_ notification: Notification) {
        Task { await PushManager.shared.foreground() }
    }

    private func startPolling() {
        poll = Task { @MainActor in
            while !Task.isCancelled {
                await AppModel.shared.refresh()
                // A continuous clock: a wait that spans sleep ends at wake, once.
                try? await Task.sleep(for: Self.pollInterval)
            }
        }
    }

    func application(_ application: NSApplication, open urls: [URL]) {
        // heraldo://event/<id> opens one event.
        for url in urls where url.scheme == "heraldo" && url.host() == "event" {
            if let id = url.pathComponents.dropFirst().first { AppModel.shared.openEvent(id) }
        }
    }

    func application(_ application: NSApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        PushManager.shared.didRegister(token: deviceToken)
    }

    func application(_ application: NSApplication, didFailToRegisterForRemoteNotificationsWithError error: Error) {
        PushManager.shared.didFail(error)
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                            withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        // A push while the app is frontmost: show it, and sync, since it means something new.
        Task { @MainActor in await AppModel.shared.refresh() }
        completionHandler([.banner, .list, .sound])
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
                                            withCompletionHandler completionHandler: @escaping () -> Void) {
        Task { @MainActor in
            await PushManager.shared.handle(response)
            await AppModel.shared.refresh()
            completionHandler()
        }
    }
}

/// The menu bar item: a bell and the unread count. It is always on screen, so
/// it also hands the model the action that opens the history window.
struct MenuBarLabel: View {
    @Environment(AppModel.self) private var model
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        let n = model.unread.badge
        HStack(spacing: 2) {
            Image(systemName: attention ? "bell.badge" : "bell")
            if n > 0 { Text("\(n)") }
        }
        .accessibilityLabel(n > 0 ? "Heraldo, \(n) unread" : "Heraldo")
        .onAppear {
            model.showMain = {
                DockPolicy.showIcon()
                openWindow(id: MacAppDelegate.mainWindowID)
                NSApp.activate()
            }
        }
    }

    private var attention: Bool {
        model.unread.badge > 0 || model.unackedIncidents > 0 || model.downChecks > 0
    }
}

/// The menu bar window: unread count, open incidents, down checks.
struct MenuBarContent: View {
    @Environment(AppModel.self) private var model
    @State private var ackError: String?

    private let limit = 6

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            header
            if !model.isConfigured {
                Text("Enter the server URL and tokens in Settings.")
                    .foregroundStyle(.secondary)
            } else {
                section("Open incidents", model.incidents, empty: "No open incidents")
                downSection
            }
            if let error = ackError ?? model.lastError {
                Text(error).font(.caption).foregroundStyle(.red).lineLimit(3)
            }
            Divider()
            footer
        }
        .padding(12)
        .frame(width: 340)
    }

    private var header: some View {
        HStack {
            Text(AppInfo.displayName()).font(.headline)
            Spacer()
            if model.isSyncing { ProgressView().controlSize(.small) }
            let n = model.unread.badge
            Text(n == 1 ? "1 unread" : "\(n) unread")
                .foregroundStyle(.secondary)
        }
    }

    @ViewBuilder
    private func section(_ title: String, _ events: [Event], empty: String) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(title).font(.subheadline.weight(.semibold)).foregroundStyle(.secondary)
            if events.isEmpty {
                Text(empty).font(.callout).foregroundStyle(.tertiary)
            }
            ForEach(events.prefix(limit)) { e in
                row(e)
            }
            if events.count > limit {
                Text("\(events.count - limit) more").font(.caption).foregroundStyle(.secondary)
            }
        }
    }

    /// The checks that are down, from /v1/checkins, like the Check-ins tab.
    private var downSection: some View {
        let down = model.down
        return VStack(alignment: .leading, spacing: 4) {
            Text("Down checks").font(.subheadline.weight(.semibold)).foregroundStyle(.secondary)
            if down.isEmpty {
                Text("No checks down").font(.callout).foregroundStyle(.tertiary)
            }
            ForEach(down.prefix(limit)) { c in
                checkRow(c)
            }
            if down.count > limit {
                Text("\(down.count - limit) more").font(.caption).foregroundStyle(.secondary)
            }
        }
    }

    private func checkRow(_ c: Check) -> some View {
        Button {
            model.showMain?()
        } label: {
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Image(systemName: "clock.badge.xmark").foregroundStyle(.red).font(.body)
                VStack(alignment: .leading, spacing: 1) {
                    Text(c.key).lineLimit(1)
                    Text([c.stateLabel, c.host].compactMap { $0 }.joined(separator: " · "))
                        .font(.caption).foregroundStyle(.secondary).lineLimit(1)
                }
                Spacer(minLength: 4)
                if let d = c.stateSince {
                    Text(d, format: .relative(presentation: .numeric, unitsStyle: .narrow))
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }

    private func row(_ e: Event) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            Button {
                model.openEvent(e.id)
            } label: {
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    KindIcon(event: e).font(.body)
                    VStack(alignment: .leading, spacing: 1) {
                        Text(e.headline).lineLimit(1)
                        Text([e.source, e.host].compactMap { $0 }.joined(separator: " · "))
                            .font(.caption).foregroundStyle(.secondary).lineLimit(1)
                    }
                    Spacer(minLength: 4)
                    if let d = e.date {
                        Text(d, format: .relative(presentation: .numeric, unitsStyle: .narrow))
                            .font(.caption).foregroundStyle(.secondary)
                    }
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            if e.isOpenIncident && !e.isAcked && !(model.credentials.writeToken?.isEmpty ?? true) {
                Button("Ack") {
                    Task {
                        do { try await model.ack(e); ackError = nil } catch { ackError = error.localizedDescription }
                    }
                }
                .controlSize(.small)
            } else if e.isAcked {
                Text("Acked").font(.caption).foregroundStyle(.secondary)
            }
        }
    }

    private var footer: some View {
        HStack {
            Button("Open Heraldo") { model.showMain?() }
            Button("Mark all read") { Task { await model.markAllRead() } }
                .disabled(!model.isConfigured || model.unread.badge == 0)
            Spacer()
            Menu {
                Button("Sync now") { Task { await model.refresh() } }
                    .disabled(!model.isConfigured)
                SettingsLink { Text("Settings…") }
                Divider()
                Button("Quit Heraldo") { NSApp.terminate(nil) }
            } label: {
                Image(systemName: "ellipsis.circle")
            }
            .menuStyle(.borderlessButton)
            .menuIndicator(.hidden)
            .fixedSize()
            .accessibilityLabel("More")
        }
    }
}
