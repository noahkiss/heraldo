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
            guard phase == .active else { return }
            Task { await model.refresh() }
            Task { await PushManager.shared.foreground() }
        }
        #else
        // The history window. One of it; the menu bar item and notifications bring it up.
        Window(AppInfo.displayName(), id: MacAppDelegate.mainWindowID) {
            RootView()
                .environment(model)
        }
        .defaultSize(width: 720, height: 560)
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
    @State private var tab = Tab.inbox
    #if os(macOS)
    @Environment(\.openSettings) private var openSettings
    #endif

    enum Tab: Hashable { case inbox, incidents, settings }

    var body: some View {
        TabView(selection: $tab) {
            InboxView()
                .tabItem { Label("Inbox", systemImage: "tray") }
                .badge(model.unread.badge)
                .tag(Tab.inbox)
            IncidentsView()
                .tabItem { Label("Incidents", systemImage: "exclamationmark.triangle") }
                .badge(model.unackedIncidents)
                .tag(Tab.incidents)
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
            openSettings()
            #endif
        }
        // The scene is often active before onChange(of: scenePhase) can see it.
        .task { await model.refresh() }
        // heraldo://event/<id> opens one event.
        .onOpenURL { url in
            guard url.scheme == "heraldo", url.host() == "event", let id = url.pathComponents.dropFirst().first else { return }
            model.openEventID = id
        }
        .onChange(of: model.openEventID) { _, id in if id != nil { tab = .inbox } }
        #if os(macOS)
        .frame(minWidth: 480, minHeight: 360)
        #endif
    }
}
