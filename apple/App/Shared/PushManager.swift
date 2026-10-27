import HeraldoKit
import Observation
import SwiftUI
import UserNotifications
#if os(iOS)
import ActivityKit
import UIKit
#else
import AppKit
#endif

/// Push on iOS and macOS: the notification categories, the APNs token, and the device
/// registration with its permission health. It registers on every launch, and
/// again on foreground when a permission changed since the last report.
@MainActor
@Observable
final class PushManager {
    static let shared = PushManager()

    private(set) var authorization: UNAuthorizationStatus = .notDetermined
    private(set) var permissions = Permissions()
    private(set) var apnsToken: String?
    private(set) var device: Device?
    private(set) var lastError: String?
    let environment: APNsEnvironment

    private let defaults = UserDefaults.standard
    private var registeredThisLaunch = false

    private var deviceID: String? {
        get { defaults.string(forKey: "deviceID") }
        set { defaults.set(newValue, forKey: "deviceID") }
    }

    /// The token and permissions the server last heard, so a foreground
    /// registers only when one of them changed.
    private var reported: DeviceRegistration? {
        get { defaults.data(forKey: "reported").flatMap { try? JSONDecoder().decode(DeviceRegistration.self, from: $0) } }
        set { defaults.set(newValue.flatMap { try? JSONEncoder().encode($0) }, forKey: "reported") }
    }

    init() {
        #if targetEnvironment(simulator)
        let simulator = true
        #else
        let simulator = false
        #endif
        #if os(iOS)
        let profileURL = Bundle.main.url(forResource: "embedded", withExtension: "mobileprovision")
        #else
        let profileURL: URL? = Bundle.main.bundleURL.appending(path: "Contents/embedded.provisionprofile")
        #endif
        let profile = profileURL.flatMap { try? Data(contentsOf: $0) }
        environment = APNs.environment(profile: profile, simulator: simulator)
    }

    /// At launch: categories, the permission prompt, and the APNs token.
    func start() {
        let center = UNUserNotificationCenter.current()
        center.setNotificationCategories(Self.categories)
        // The token does not wait for the answer; the registration reports the
        // permissions as they are and again once the person answers.
        #if os(iOS)
        UIApplication.shared.registerForRemoteNotifications()
        #else
        NSApplication.shared.registerForRemoteNotifications()
        #endif
        Task {
            await readPermissions()
            _ = try? await center.requestAuthorization(options: [.alert, .badge, .sound])
            await readPermissions()
            await register(force: false)
        }
    }

    static var categories: Set<UNNotificationCategory> {
        let ack = UNNotificationAction(identifier: PushCategory.ackAction, title: "Acknowledge",
                                       options: [.authenticationRequired],
                                       icon: UNNotificationActionIcon(systemImageName: "hand.raised"))
        let open = UNNotificationAction(identifier: PushCategory.openAction, title: "Open",
                                        options: [.foreground],
                                        icon: UNNotificationActionIcon(systemImageName: "arrow.up.forward.app"))
        return [
            UNNotificationCategory(identifier: PushCategory.incident, actions: [ack, open], intentIdentifiers: []),
            UNNotificationCategory(identifier: PushCategory.event, actions: [open], intentIdentifiers: []),
        ]
    }

    func didRegister(token: Data) {
        apnsToken = APNs.hex(token)
        log.info("push: APNs token received")
        Task { await register(force: !registeredThisLaunch) }
    }

    func didFail(_ error: Error) {
        lastError = "APNs: \(error.localizedDescription)"
        log.error("push: APNs registration failed: \(error.localizedDescription, privacy: .public)")
    }

    /// On foreground: report permissions again when they changed.
    func foreground() async {
        await readPermissions()
        await register(force: false)
    }

    func readPermissions() async {
        let s = await UNUserNotificationCenter.current().notificationSettings()
        authorization = s.authorizationStatus
        var p = Permissions()
        switch s.authorizationStatus {
        case .notDetermined:
            break // unknown until the person answers
        case .denied:
            p.alerts = false
            p.timeSensitive = false
        default:
            p.alerts = s.alertSetting == .enabled
            if s.timeSensitiveSetting != .notSupported {
                p.timeSensitive = s.timeSensitiveSetting == .enabled
            }
        }
        #if os(iOS)
        p.liveActivities = ActivityAuthorizationInfo().areActivitiesEnabled
        #endif
        permissions = p
    }

    /// POST /v1/devices, or PUT when the APNs token changed under a known id.
    func register(force: Bool) async {
        let model = AppModel.shared
        guard let token = apnsToken, model.isConfigured, let client = try? model.client() else { return }
        let reg = DeviceRegistration(
            token: token, environment: environment, topic: Bundle.main.bundleIdentifier ?? "",
            name: Self.deviceName, platform: Self.platform, appVersion: AppInfo.version(),
            permissions: permissions)
        let last = reported
        guard force || last != reg else { return }
        do {
            var dev: Device
            if let id = deviceID, let last, last.token != token {
                do {
                    dev = try await client.updateDevice(id: id, reg)
                } catch let e as HeraldoError where e.status == 404 || e.status == 409 {
                    dev = try await client.register(reg)
                }
            } else {
                dev = try await client.register(reg)
            }
            device = dev
            deviceID = dev.id
            reported = reg
            registeredThisLaunch = true
            lastError = nil
            log.info("push: registered device \(dev.id, privacy: .public) (\(self.environment.rawValue, privacy: .public))")
        } catch {
            lastError = "Registration: \(error.localizedDescription)"
            log.error("push: registration failed: \(error.localizedDescription, privacy: .public)")
        }
    }

    #if os(iOS)
    static var deviceName: String { UIDevice.current.name }
    static let platform = "iOS"
    #else
    static var deviceName: String { Host.current().localizedName ?? "Mac" }
    static let platform = "macOS"
    #endif

    /// Settings changed: register with the (maybe new) server now.
    func credentialsChanged() {
        reported = nil
        Task { await register(force: true) }
    }

    func setBadge(_ n: Int) {
        #if os(iOS)
        UNUserNotificationCenter.current().setBadgeCount(n)
        #else
        // The Dock tile shows the count whether or not notifications are allowed.
        NSApp.dockTile.badgeLabel = n > 0 ? "\(n)" : nil
        #endif
    }

    // MARK: Notification responses

    func handle(_ response: UNNotificationResponse) async {
        let userInfo = response.notification.request.content.userInfo
        guard let info = PushInfo(userInfo: userInfo) else { return }
        let model = AppModel.shared
        switch response.actionIdentifier {
        case PushCategory.ackAction:
            await ack(info)
        case PushCategory.openAction:
            if let url = info.url.flatMap(URL.init(string:)) {
                #if os(iOS)
                await UIApplication.shared.open(url)
                #else
                NSWorkspace.shared.open(url)
                #endif
            } else {
                model.openEvent(info.id)
            }
        case UNNotificationDefaultActionIdentifier:
            model.openEvent(info.id)
        default:
            break
        }
    }

    private func ack(_ info: PushInfo) async {
        let model = AppModel.shared
        do {
            let client = try model.client()
            // The full event gives the ack its title and host.
            var incident = (try? await client.event(id: info.id)) ?? Event(id: info.id, source: info.source ?? "heraldo")
            if incident.key == nil { incident.key = info.key }
            try await client.ack(incident, source: AppInfo.source)
            await model.refresh()
        } catch {
            let content = UNMutableNotificationContent()
            content.title = "Acknowledge failed"
            content.body = error.localizedDescription
            content.interruptionLevel = .passive
            try? await UNUserNotificationCenter.current().add(
                UNNotificationRequest(identifier: "ack-failed-\(info.id)", content: content, trigger: nil))
        }
    }
}

/// The notifications part of Settings.
struct PushSection: View {
    @State private var push = PushManager.shared
    @Environment(\.openURL) private var openURL

    var body: some View {
        Section {
            LabeledContent("Notifications", value: authorization)
            if push.authorization == .notDetermined {
                Button("Allow notifications") { push.start() }
            } else if push.authorization == .denied || push.permissions.timeSensitive == false {
                Button("Open notification settings") {
                    if let url = Self.notificationSettingsURL { openURL(url) }
                }
            }
            permission("Alerts", push.permissions.alerts)
            permission("Time Sensitive", push.permissions.timeSensitive)
            #if os(iOS)
            permission("Live Activities", push.permissions.liveActivities)
            #endif
            LabeledContent("APNs", value: push.environment.rawValue)
            if let d = push.device {
                LabeledContent("Device", value: d.id)
                LabeledContent("Token", value: "…" + d.tokenSuffix)
            } else if push.apnsToken == nil {
                LabeledContent("Device", value: "No APNs token yet")
            } else {
                LabeledContent("Device", value: "Not registered")
            }
            if let error = push.lastError {
                Text(error).font(.footnote).foregroundStyle(.red)
            }
            Button("Register again") { Task { await push.register(force: true) } }
                .disabled(push.apnsToken == nil)
        } header: {
            Text("Push")
        }
    }

    #if os(iOS)
    private static let notificationSettingsURL = URL(string: UIApplication.openNotificationSettingsURLString)
    #else
    private static let notificationSettingsURL = URL(
        string: "x-apple.systempreferences:com.apple.Notifications-Settings.extension?id=\(Bundle.main.bundleIdentifier ?? "")")
    #endif

    private var authorization: String {
        switch push.authorization {
        case .authorized: return "Allowed"
        case .denied: return "Off"
        case .provisional: return "Quiet"
        case .ephemeral: return "Temporary"
        case .notDetermined: return "Not asked"
        @unknown default: return "Unknown"
        }
    }

    private func permission(_ name: String, _ value: Bool?) -> some View {
        LabeledContent(name) {
            switch value {
            case true?: Image(systemName: "checkmark.circle.fill").foregroundStyle(.green).accessibilityLabel("On")
            case false?: Image(systemName: "xmark.circle.fill").foregroundStyle(.red).accessibilityLabel("Off")
            case nil: Text("Unknown").foregroundStyle(.secondary)
            }
        }
    }
}
