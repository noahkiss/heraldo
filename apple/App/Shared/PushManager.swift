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
/// Registrations run one at a time, so the launch triggers do not each post.
@MainActor
@Observable
final class PushManager {
    static let shared = PushManager()

    private(set) var authorization: UNAuthorizationStatus = .notDetermined
    private(set) var permissions = Permissions()
    /// Live Activities may update often: ActivityKit's frequentPushesEnabled.
    private(set) var frequentPushes: Bool?
    private(set) var apnsToken: String?
    private(set) var device: Device?
    private(set) var lastError: String?
    let environment: APNsEnvironment

    private let defaults = UserDefaults.standard
    private var registeredThisLaunch = false
    /// The registration running now; the next one waits for it.
    private var registerTask: Task<Void, Never>?

    private var deviceID: String? {
        get { defaults.string(forKey: "deviceID") }
        set { defaults.set(newValue, forKey: "deviceID") }
    }

    /// The server's id for this device, once registered.
    var deviceIdentifier: String? { deviceID }

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

    /// Whether this build has the critical-alerts entitlement:
    /// HERALDO_CRITICAL_ALERTS = YES in Names.xcconfig.
    #if HERALDO_CRITICAL_ALERTS
    static let criticalAlertsBuilt = true
    #else
    static let criticalAlertsBuilt = false
    #endif

    /// What the app asks for: .criticalAlert only in a build with the
    /// entitlement, which makes iOS show its own critical-alerts prompt.
    static var authorizationOptions: UNAuthorizationOptions {
        var o: UNAuthorizationOptions = [.alert, .badge, .sound]
        #if HERALDO_CRITICAL_ALERTS
        o.insert(.criticalAlert)
        #endif
        return o
    }

    /// At launch: categories and the APNs token. The first prompt comes from
    /// the permissions sheet; after that, a launch asks again, which shows
    /// nothing unless the options grew (a build that gained .criticalAlert).
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
            if PermissionsSheet.seen {
                _ = try? await center.requestAuthorization(options: Self.authorizationOptions)
                await readPermissions()
            }
            await register(force: false)
        }
    }

    /// The Notifications row of the permissions sheet: the system prompt,
    /// then a fresh report to the server.
    func requestAuthorization() async {
        _ = try? await UNUserNotificationCenter.current().requestAuthorization(options: Self.authorizationOptions)
        await readPermissions()
        await register(force: false)
    }

    /// After a row's answer: read the permissions and report them.
    func refreshPermissions() async {
        await readPermissions()
        await register(force: false)
    }

    static var categories: Set<UNNotificationCategory> {
        let ack = UNNotificationAction(identifier: PushCategory.ackAction, title: "Acknowledge",
                                       options: [.authenticationRequired],
                                       icon: UNNotificationActionIcon(systemImageName: "hand.raised"))
        let mute = UNNotificationAction(identifier: PushCategory.muteAction, title: "Mute 1 h",
                                        options: [.authenticationRequired],
                                        icon: UNNotificationActionIcon(systemImageName: "bell.slash"))
        let snooze = UNNotificationAction(identifier: PushCategory.snoozeAction, title: "Snooze 1 h",
                                          options: [.authenticationRequired],
                                          icon: UNNotificationActionIcon(systemImageName: "clock.arrow.circlepath"))
        let open = UNNotificationAction(identifier: PushCategory.openAction, title: "Open",
                                        options: [.foreground],
                                        icon: UNNotificationActionIcon(systemImageName: "arrow.up.forward.app"))
        return [
            UNNotificationCategory(identifier: PushCategory.incident, actions: [ack, snooze, mute, open], intentIdentifiers: []),
            UNNotificationCategory(identifier: PushCategory.event, actions: [mute, open], intentIdentifiers: []),
        ]
    }

    func didRegister(token: Data) {
        apnsToken = APNs.hex(token)
        log.info("push: APNs token received")
        // Forced only when nothing registered this launch yet, judged when
        // the registration runs, not now.
        Task { await register(force: false, firstThisLaunch: true) }
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
            #if HERALDO_CRITICAL_ALERTS
            p.criticalAlerts = false
            #endif
        default:
            p.alerts = s.alertSetting == .enabled
            if s.timeSensitiveSetting != .notSupported {
                p.timeSensitive = s.timeSensitiveSetting == .enabled
            }
            #if HERALDO_CRITICAL_ALERTS
            if s.criticalAlertSetting != .notSupported {
                p.criticalAlerts = s.criticalAlertSetting == .enabled
            }
            #endif
        }
        #if os(iOS)
        let la = ActivityAuthorizationInfo()
        p.liveActivities = la.areActivitiesEnabled
        frequentPushes = la.frequentPushesEnabled
        #endif
        permissions = p
    }

    /// POST /v1/devices, or PUT when the APNs token changed under a known id.
    /// One runs at a time: a call waits for the one before it, then skips
    /// when the server already has this registration. `firstThisLaunch`
    /// forces it when no registration succeeded this launch yet.
    func register(force: Bool, firstThisLaunch: Bool = false) async {
        let previous = registerTask
        let task = Task {
            await previous?.value
            await self.doRegister(force: force || (firstThisLaunch && !self.registeredThisLaunch))
        }
        registerTask = task
        await task.value
    }

    private func doRegister(force: Bool) async {
        let model = AppModel.shared
        guard let token = apnsToken, model.isConfigured, let client = try? model.client() else { return }
        #if os(iOS)
        let liveToken = LiveActivities.shared.pushToStartToken
        #else
        let liveToken: String? = nil
        #endif
        let reg = DeviceRegistration(
            token: token, environment: environment, topic: Bundle.main.bundleIdentifier ?? "",
            name: Self.deviceName, platform: Self.platform, appVersion: AppInfo.version(),
            permissions: permissions, liveActivityToken: liveToken)
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
            if dev.id != deviceID { reportedFocus = nil }
            device = dev
            deviceID = dev.id
            reported = reg
            registeredThisLaunch = true
            lastError = nil
            log.info("push: registered device \(dev.id, privacy: .public) (\(self.environment.rawValue, privacy: .public))")
            if FocusFilterStore.load() != reportedFocus { await reportFocus() }
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

    // MARK: Focus filter

    /// The Focus filter the server last took for this device.
    private var reportedFocus: FocusFilterState? {
        get { defaults.data(forKey: "reportedFocus").flatMap { try? JSONDecoder().decode(FocusFilterState.self, from: $0) } }
        set { defaults.set(newValue.flatMap { try? JSONEncoder().encode($0) }, forKey: "reportedFocus") }
    }

    /// Sends the Focus filter in force (FocusFilterStore) to the server with
    /// PUT /v1/devices/{id}. Only this call sends focus_filter: a server
    /// before 0.7 refuses the field with 400, and the usual registration
    /// must keep working there.
    func reportFocus() async {
        let filter = FocusFilterStore.load()
        guard let filter, filter != reportedFocus else { return }
        guard let id = deviceID, var reg = reported, let client = try? AppModel.shared.client() else { return }
        reg.focusFilter = filter
        do {
            device = try await client.updateDevice(id: id, reg)
            reportedFocus = filter
            log.info("push: Focus filter \(filter.active ? "on" : "off", privacy: .public)")
        } catch let e as HeraldoError where e.status == 400 {
            log.info("push: the server takes no Focus filter: \(e.localizedDescription, privacy: .public)")
        } catch {
            log.error("push: Focus filter: \(error.localizedDescription, privacy: .public)")
        }
    }

    /// Settings changed: register with the (maybe new) server now.
    func credentialsChanged() {
        reported = nil
        reportedFocus = nil
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
        case PushCategory.muteAction:
            await mute(info)
        case PushCategory.snoozeAction:
            await snooze(info)
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

    /// Mute 1 h: the incident's key, or the event's source.
    private func mute(_ info: PushInfo) async {
        guard let input = PushCategory.muteInput(info) else { return }
        do {
            let m = try await AppModel.shared.client().mute(input)
            log.info("push: muted \(m.key ?? m.source ?? "", privacy: .public) until \(m.until, privacy: .public)")
        } catch {
            await failed("Mute failed", error, id: "mute-failed-\(info.id)")
        }
    }

    /// Snooze 1 h: mutes the incident's key, and the server pushes it again
    /// in an hour if it is still open.
    private func snooze(_ info: PushInfo) async {
        guard let input = PushCategory.snoozeInput(info) else { return }
        do {
            let m = try await AppModel.shared.client().mute(input)
            log.info("push: snoozed \(m.key ?? "", privacy: .public) until \(m.until, privacy: .public)")
        } catch {
            await failed("Snooze failed", error, id: "snooze-failed-\(info.id)")
        }
    }

    private func failed(_ title: String, _ error: Error, id: String) async {
        let content = UNMutableNotificationContent()
        content.title = title
        content.body = error.localizedDescription
        content.interruptionLevel = .passive
        try? await UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: id, content: content, trigger: nil))
    }

    private func ack(_ info: PushInfo) async {
        let model = AppModel.shared
        do {
            let client = try model.client()
            // The full event gives the ack its title and host.
            var incident = (try? await client.event(id: info.id)) ?? Event(id: info.id, source: info.source ?? "heraldo")
            if incident.key == nil { incident.key = info.key }
            try await client.ack(incident, source: AppInfo.source)
            await model.refresh([.state, .history, .unread])
        } catch {
            await failed("Acknowledge failed", error, id: "ack-failed-\(info.id)")
        }
    }
}

/// The notifications part of Settings.
struct PushSection: View {
    @State private var push = PushManager.shared
    @State private var showPermissions = false
    @Environment(\.openURL) private var openURL

    var body: some View {
        Section {
            LabeledContent("Notifications", value: authorization)
            Button("Permissions…") { showPermissions = true }
            if push.authorization == .notDetermined {
                Button("Allow notifications") { Task { await push.requestAuthorization() } }
            } else if push.authorization == .denied || push.permissions.timeSensitive == false {
                Button("Open notification settings") {
                    if let url = Self.notificationSettingsURL { openURL(url) }
                }
            }
            permission("Alerts", push.permissions.alerts)
            permission("Time Sensitive", push.permissions.timeSensitive)
            if PushManager.criticalAlertsBuilt {
                permission("Critical Alerts", push.permissions.criticalAlerts)
            }
            #if os(iOS)
            permission("Live Activities", push.permissions.liveActivities)
            #endif
            LabeledContent("APNs", value: push.environment.rawValue)
            if let d = push.device {
                LabeledContent("Device", value: d.id)
                LabeledContent("Token", value: "…" + d.tokenSuffix)
                LabeledContent("Focus filter", value: Self.focusText(d.focusFilter))
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
        .sheet(isPresented: $showPermissions) { PermissionsSheet() }
    }

    private static var notificationSettingsURL: URL? { PermissionsSheet.notificationSettingsURL }

    private static func focusText(_ f: FocusFilterState?) -> String {
        guard let f, f.active else { return "Off" }
        let level = f.breakThrough == FocusFilterState.critical ? "Critical only" : "Time Sensitive and up"
        guard let p = f.projects, !p.isEmpty else { return level }
        return "\(level), \(p.joined(separator: ", "))"
    }

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
