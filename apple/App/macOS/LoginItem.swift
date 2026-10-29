import ServiceManagement
import SwiftUI

/// Opens the app at login, through SMAppService. The system owns the
/// state: the user can also change it in System Settings > General >
/// Login Items, so the app reads it back rather than storing it.
@MainActor
enum LoginItem {
    /// Set once the app has turned the login item on by itself.
    private static let offeredKey = "loginItemOffered"

    static var isEnabled: Bool { SMAppService.mainApp.status == .enabled }
    static var needsApproval: Bool { SMAppService.mainApp.status == .requiresApproval }

    /// The first launch turns it on: an alert hub that is not running
    /// misses nothing on the server, but shows nothing either. The user
    /// turns it off in Settings, and it stays off.
    static func start() {
        guard !UserDefaults.standard.bool(forKey: offeredKey) else { return }
        UserDefaults.standard.set(true, forKey: offeredKey)
        set(true)
    }

    static func set(_ on: Bool) {
        do {
            if on { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() }
        } catch {
            log.error("login item: \(error.localizedDescription, privacy: .public)")
        }
    }
}

/// The Settings row for the login item.
struct LoginItemSection: View {
    @State private var on = LoginItem.isEnabled
    @State private var needsApproval = LoginItem.needsApproval

    var body: some View {
        Section("Login") {
            Toggle("Open at login", isOn: $on)
                .onChange(of: on) { _, value in
                    LoginItem.set(value)
                    reload()
                }
            if needsApproval {
                Button("Allow in System Settings") { SMAppService.openSystemSettingsLoginItems() }
            }
        }
        .onAppear(perform: reload)
    }

    private func reload() {
        let enabled = LoginItem.isEnabled
        if on != enabled { on = enabled }
        needsApproval = LoginItem.needsApproval
    }
}
