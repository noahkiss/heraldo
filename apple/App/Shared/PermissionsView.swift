import SwiftUI
import UserNotifications
#if os(iOS)
import UIKit
#endif

/// The permissions sheet: shown once at first launch, and from Settings >
/// Push > Permissions. One row per permission, each with its state and one
/// button. After each answer the app reports the permissions to the server.
struct PermissionsSheet: View {
    @State private var push = PushManager.shared
    @Environment(\.dismiss) private var dismiss
    @Environment(\.openURL) private var openURL
    @Environment(\.scenePhase) private var scenePhase

    /// Whether the sheet was shown and closed once.
    static var seen: Bool {
        get { UserDefaults.standard.bool(forKey: seenKey) }
        set { UserDefaults.standard.set(newValue, forKey: seenKey) }
    }

    static let seenKey = "permissionsSheetSeen"

    #if os(iOS)
    static let notificationSettingsURL = URL(string: UIApplication.openNotificationSettingsURLString)
    static let appSettingsURL = URL(string: UIApplication.openSettingsURLString)
    #else
    static let notificationSettingsURL = URL(
        string: "x-apple.systempreferences:com.apple.Notifications-Settings.extension?id=\(Bundle.main.bundleIdentifier ?? "")")
    #endif

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    row("Notifications", state: notifications) {
                        if push.authorization == .notDetermined {
                            Button("Allow") { Task { await push.requestAuthorization() } }
                        } else {
                            settingsButton()
                        }
                    }
                    row("Time Sensitive", state: push.permissions.timeSensitive) {
                        settingsButton()
                    }
                    if PushManager.criticalAlertsBuilt {
                        row("Critical Alerts", state: push.permissions.criticalAlerts) {
                            if push.authorization == .notDetermined {
                                Button("Allow") { Task { await push.requestAuthorization() } }
                            } else {
                                settingsButton()
                            }
                        }
                    }
                    #if os(iOS)
                    row("Live Activities", state: push.permissions.liveActivities) {
                        Button("Open Settings") {
                            if let url = Self.appSettingsURL { openURL(url) }
                        }
                    }
                    if push.permissions.liveActivities == true, push.frequentPushes == false {
                        row("Frequent Updates", state: false) {
                            Button("Open Settings") {
                                if let url = Self.appSettingsURL { openURL(url) }
                            }
                        }
                    }
                    #endif
                } footer: {
                    Text(footer)
                }
            }
            .formStyle(.grouped)
            .navigationTitle("Permissions")
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done") { dismiss() }
                }
            }
        }
        #if os(macOS)
        .frame(minWidth: 420, minHeight: 320)
        #endif
        .task { await push.refreshPermissions() }
        // Back from Settings: read the answers again.
        .onChange(of: scenePhase) { _, phase in
            if phase == .active { Task { await push.refreshPermissions() } }
        }
        .onDisappear { Self.seen = true }
    }

    private var footer: String { "Turn on Time Sensitive in Settings." }

    private func settingsButton() -> some View {
        Button("Open Settings") {
            if let url = Self.notificationSettingsURL { openURL(url) }
        }
    }

    private var notifications: Bool? {
        switch push.authorization {
        case .notDetermined: return nil
        case .denied: return false
        default: return push.permissions.alerts ?? true
        }
    }


    private func row(_ name: String, state: Bool?, @ViewBuilder action: () -> some View) -> some View {
        HStack {
            VStack(alignment: .leading) {
                Text(name)
                Text(label(state)).font(.footnote).foregroundStyle(.secondary)
            }
            Spacer()
            if state != true { action() }
            else {
                Image(systemName: "checkmark.circle.fill").foregroundStyle(.green).accessibilityLabel("On")
            }
        }
    }

    private func label(_ v: Bool?) -> String {
        switch v {
        case true?: return "On"
        case false?: return "Off"
        case nil: return "Not asked"
        }
    }
}

extension View {
    /// Shows the permissions sheet once, at first launch.
    func permissionsOnboarding() -> some View { modifier(PermissionsOnboarding()) }
}

struct PermissionsOnboarding: ViewModifier {
    @State private var show = false

    func body(content: Content) -> some View {
        content
            .onAppear { if !PermissionsSheet.seen { show = true } }
            .sheet(isPresented: $show) { PermissionsSheet() }
    }
}
