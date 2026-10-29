import HeraldoKit
import SwiftUI

@main
struct HeraldoApp: App {
    #if os(iOS)
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    #else
    @NSApplicationDelegateAdaptor(MacAppDelegate.self) private var delegate
    #endif
    @Environment(\.scenePhase) private var scenePhase
    @State private var model = AppModel.shared

    var body: some Scene {
        #if os(iOS)
        WindowGroup {
            RootView()
                .environment(model)
        }
        .onChange(of: scenePhase) { _, phase in
            switch phase {
            case .active:
                AppSync.shared.sceneActive()
                Task { await PushManager.shared.foreground() }
                Task { await LiveActivities.shared.flush() }
            case .background:
                AppSync.shared.sceneBackground()
            default:
                break
            }
        }
        #else
        // The main window. One of it; the menu bar item and notifications bring it up.
        Window(AppInfo.displayName(), id: MacAppDelegate.mainWindowID) {
            MacRootView()
                .environment(model)
        }
        .defaultSize(width: 1120, height: 720)
        .commands { MacCommands() }
        // One event, incident, check, activity or held entry in its own window.
        WindowGroup("Detail", id: MacAppDelegate.detailWindowID, for: MacDetailItem.self) { $item in
            MacDetailWindow(item: item)
                .environment(model)
        }
        .defaultSize(width: 560, height: 640)
        MenuBarExtra {
            MenuBarContent()
                .environment(model)
        } label: {
            MenuBarLabel()
                .environment(model)
        }
        .menuBarExtraStyle(.window)
        Settings {
            SettingsView()
                .environment(model)
                .frame(width: 460)
        }
        #endif
    }
}

struct RootView: View {
    @Environment(AppModel.self) private var model
    @State private var tab = DisplayPrefs.shared.defaultView.flatMap(Tab.init(rawValue:)) ?? .inbox
    #if os(macOS)
    @Environment(\.openSettings) private var openSettings
    #endif

    /// The raw values are the synced default view's names.
    enum Tab: String, Hashable { case inbox, incidents, checkins, settings }

    var body: some View {
        TabView(selection: $tab) {
            InboxView()
                .tabItem { Label("Inbox", systemImage: "tray") }
                .badge(model.unread.inboxCount)
                .tag(Tab.inbox)
            IncidentsView()
                .tabItem { Label("Incidents", systemImage: "exclamationmark.triangle") }
                .badge(model.unackedIncidents)
                .tag(Tab.incidents)
            CheckinsView()
                .tabItem { Label("Check-ins", systemImage: "clock.badge.checkmark") }
                .badge(model.downChecks)
                .tag(Tab.checkins)
            #if os(iOS)
            SettingsView()
                .tabItem { Label("Settings", systemImage: "gearshape") }
                .tag(Tab.settings)
            #endif
        }
        .onAppear {
            guard !model.isConfigured else { return }
            #if os(iOS)
            tab = .settings
            #else
            // A provisioning launch saves the settings itself.
            if !Provision.requested { openSettings() }
            #endif
        }
        #if os(iOS)
        // The scene is often active before onChange(of: scenePhase) can see it.
        .task { AppSync.shared.sceneActive() }
        #endif
        .heraldoLinks()
        .permissionsOnboarding()
        .onChange(of: model.openEventID) { _, id in if id != nil { tab = .inbox } }
    }
}

extension View {
    /// heraldo://event/<id> opens one event. heraldo://open?url=<url> opens
    /// the URL: a Live Activity's open button.
    func heraldoLinks() -> some View { modifier(HeraldoLinks()) }
}

struct HeraldoLinks: ViewModifier {
    @Environment(AppModel.self) private var model
    @Environment(\.openURL) private var openURL

    func body(content: Content) -> some View {
        content.onOpenURL { url in
            guard url.scheme == "heraldo" else { return }
            if url.host() == "open",
               let target = URLComponents(url: url, resolvingAgainstBaseURL: false)?
                   .queryItems?.first(where: { $0.name == "url" })?.value.flatMap(URL.init(string:)) {
                openURL(target)
                return
            }
            guard url.host() == "event", let id = url.pathComponents.dropFirst().first else { return }
            model.openEventID = id
        }
    }
}
