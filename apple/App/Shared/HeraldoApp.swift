import HeraldoKit
import SwiftUI

@main
struct HeraldoApp: App {
    #if os(iOS)
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    #endif
    @Environment(\.scenePhase) private var scenePhase
    @State private var model = AppModel.shared

    var body: some Scene {
        WindowGroup {
            RootView()
                .environment(model)
        }
        .onChange(of: scenePhase) { _, phase in
            guard phase == .active else { return }
            Task { await model.refresh() }
            #if os(iOS)
            Task { await PushManager.shared.foreground() }
            #endif
        }
    }
}

struct RootView: View {
    @Environment(AppModel.self) private var model
    @State private var tab = Tab.inbox

    enum Tab: Hashable { case inbox, incidents, settings }

    var body: some View {
        TabView(selection: $tab) {
            InboxView()
                .tabItem { Label("Inbox", systemImage: "tray") }
                .badge(model.unread.badge)
                .tag(Tab.inbox)
            IncidentsView()
                .tabItem { Label("Incidents", systemImage: "exclamationmark.triangle") }
                .badge(model.incidents.filter { $0.action != "ack" }.count)
                .tag(Tab.incidents)
            SettingsView()
                .tabItem { Label("Settings", systemImage: "gearshape") }
                .tag(Tab.settings)
        }
        .onAppear { if !model.isConfigured { tab = .settings } }
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
