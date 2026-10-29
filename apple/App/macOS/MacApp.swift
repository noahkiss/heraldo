import AppKit
import HeraldoKit
import SwiftUI
import UserNotifications

/// The Mac app's delegate: push, notification responses, heraldo:// links,
/// and the start of `AppSync`, which keeps the menu bar current while no
/// window is open.
final class MacAppDelegate: NSObject, NSApplicationDelegate, UNUserNotificationCenterDelegate {
    static let mainWindowID = "main"
    /// The windows that show one item: Return or a double-click in a list.
    static let detailWindowID = "detail"

    func applicationDidFinishLaunching(_ notification: Notification) {
        #if DEBUG
        // Screenshots in both themes: HERALDO_APPEARANCE=light or dark.
        switch ProcessInfo.processInfo.environment["HERALDO_APPEARANCE"] {
        case "light": NSApp.appearance = NSAppearance(named: .aqua)
        case "dark": NSApp.appearance = NSAppearance(named: .darkAqua)
        default: break
        }
        #endif
        UNUserNotificationCenter.current().delegate = self
        NSApp.servicesProvider = MacServices.shared
        let push = PushManager.shared
        AppModel.shared.onBadge = { push.setBadge($0) }
        AppModel.shared.onCredentialsChanged = {
            push.credentialsChanged()
            AppSync.shared.credentialsChanged()
        }
        push.start()
        if Provision.requested { Task { await Provision.run(AppModel.shared) } }
        // The stream, the safety net, sleep and wake, window visibility.
        MacSync.start()
        DockPolicy.start()
        LoginItem.start()
    }

    /// The menu bar item stays when the window closes. DockPolicy hides the Dock icon.
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        if !flag { AppModel.shared.showMain?() }
        return true
    }

    func applicationDidBecomeActive(_ notification: Notification) {
        AppSync.shared.driver?.opened()
        Task { await PushManager.shared.foreground() }
    }

    /// An alert push with content-available reaches a running app here,
    /// frontmost or not: something changed on the server.
    func application(_ application: NSApplication, didReceiveRemoteNotification userInfo: [String: Any]) {
        AppSync.shared.driver?.hint()
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
        Task { @MainActor in AppSync.shared.driver?.hint() }
        completionHandler([.banner, .list, .sound])
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
                                            withCompletionHandler completionHandler: @escaping () -> Void) {
        Task { @MainActor in
            await PushManager.shared.handle(response)
            AppSync.shared.driver?.hint()
            completionHandler()
        }
    }
}

/// The menu bar item: a bell and the badge (the server's badge.scope count,
/// as on the Dock). The bell fills and gains
/// a badge while an incident is open or a check is down. It is always on
/// screen, so it also hands the model the action that opens the main window.
struct MenuBarLabel: View {
    @Environment(AppModel.self) private var model
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        let n = model.unread.badge
        HStack(spacing: 2) {
            Image(systemName: symbol)
            if n > 0 { Text("\(n)").monospacedDigit() }
        }
        .accessibilityLabel(MenuBarSummary(model: model).accessibility)
        .onAppear {
            model.showMain = {
                DockPolicy.showIcon()
                openWindow(id: MacAppDelegate.mainWindowID)
                NSApp.activate()
            }
        }
    }

    private var symbol: String {
        if model.unackedIncidents > 0 || model.downChecks > 0 { return "bell.badge.fill" }
        return model.unread.badge > 0 ? "bell.badge" : "bell"
    }
}

/// The state in one line: open incidents, checks down, running activities.
@MainActor
struct MenuBarSummary {
    let model: AppModel

    var parts: [String] {
        var out: [String] = []
        let open = model.unackedIncidents
        if open > 0 { out.append(open == 1 ? "1 open incident" : "\(open) open incidents") }
        let down = model.downChecks
        if down > 0 { out.append(down == 1 ? "1 check down" : "\(down) checks down") }
        let running = model.activities.count
        if running > 0 { out.append(running == 1 ? "1 running" : "\(running) running") }
        let held = model.held.count
        if held > 0 { out.append("\(held) held") }
        return out
    }

    var line: String { parts.isEmpty ? "All clear" : parts.joined(separator: " · ") }

    var accessibility: String {
        let n = model.unread.badge
        var items = [AppInfo.displayName()]
        if n > 0 { items.append("badge \(n)") }
        items += parts
        return items.joined(separator: ", ")
    }
}

/// The menu bar window: unread count, open incidents, down checks and
/// running Live Activities.
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
                if !model.activities.isEmpty { runningSection }
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
        VStack(alignment: .leading, spacing: 2) {
            HStack {
                Text(AppInfo.displayName()).font(.headline)
                Spacer()
                if model.isSyncing && UILiveness.shared.menu { ProgressView().controlSize(.small) }
                let n = model.unread.inboxCount
                Text(n == 1 ? "1 unread" : "\(n) unread")
                    .monospacedDigit()
                    .foregroundStyle(.secondary)
            }
            if model.isConfigured {
                let summary = MenuBarSummary(model: model)
                Text(summary.line)
                    .font(.callout)
                    .foregroundStyle(summary.parts.isEmpty ? AnyShapeStyle(.secondary) : AnyShapeStyle(.primary))
            }
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

    /// The running Live Activities. Shown only while one runs.
    private var runningSection: some View {
        let running = model.activities
        return VStack(alignment: .leading, spacing: 4) {
            Text("Live Activities").font(.subheadline.weight(.semibold)).foregroundStyle(.secondary)
            ForEach(running.prefix(limit)) { a in
                activityRow(a)
            }
            if running.count > limit {
                Text("\(running.count - limit) more").font(.caption).foregroundStyle(.secondary)
            }
        }
    }

    private func activityRow(_ a: ActivitySummary) -> some View {
        Button {
            show(.activities, .activity(a.key))
        } label: {
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Image(systemName: a.content?.symbol ?? MacHome.activities.symbol)
                    .foregroundStyle(activityTint(a.content?.tint)).font(.body)
                VStack(alignment: .leading, spacing: 1) {
                    Text(a.headline).lineLimit(1)
                    if let line = a.content?.status ?? a.content?.body, !line.isEmpty {
                        Text(line).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                    }
                }
                Spacer(minLength: 4)
                if let text = a.content?.state.progressText {
                    Text(text).monospacedDigit().font(.caption).foregroundStyle(.secondary)
                }
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }

    /// Opens the main window on a home with one item selected.
    private func show(_ home: MacHome, _ item: MacDetailItem) {
        let window = MacWindowState.shared
        window.show(home)
        window.selection = item
        model.showMain?()
    }

    private func checkRow(_ c: Check) -> some View {
        Button {
            show(.checkins, .check(c.key))
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
                    Text(d, format: .relative(presentation: .numeric, unitsStyle: .narrow)).monospacedDigit()
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
                        Text(d, format: .relative(presentation: .numeric, unitsStyle: .narrow)).monospacedDigit()
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
                .keyboardShortcut(.defaultAction)
            Button("Mark all read") { Task { await model.markAllRead() } }
                .disabled(!model.isConfigured || model.unread.inboxCount == 0)
            Spacer()
            Menu {
                Button("Sync now") { Task { await AppSync.shared.syncNow() } }
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
