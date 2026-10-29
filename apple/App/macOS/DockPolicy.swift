import AppKit

/// Shows the Dock icon only while a window is open, when the setting is on.
/// With no window the app is an accessory: menu bar item only, no Dock icon,
/// not in Command-Tab. The Settings window counts as a window; the menu bar
/// item's panel does not.
@MainActor
enum DockPolicy {
    /// The UserDefaults key. Device-specific, so it stays out of iCloud.
    static let key = "dockOnlyWithWindow"

    static var isOn: Bool { UserDefaults.standard.bool(forKey: key) }

    static func start() {
        UserDefaults.standard.register(defaults: [key: true])
        let center = NotificationCenter.default
        for name in [NSWindow.didBecomeKeyNotification, NSWindow.didChangeOcclusionStateNotification,
                     NSWindow.didDeminiaturizeNotification] {
            center.addObserver(forName: name, object: nil, queue: .main) { _ in
                MainActor.assumeIsolated { update() }
            }
        }
        // The window is still on screen during willClose; look once it is gone.
        center.addObserver(forName: NSWindow.willCloseNotification, object: nil, queue: .main) { _ in
            DispatchQueue.main.async { update() }
        }
        center.addObserver(forName: UserDefaults.didChangeNotification, object: nil, queue: .main) { _ in
            MainActor.assumeIsolated { update() }
        }
        // A launch with no window (state restoration, a login item) hides the
        // icon once SwiftUI has made its windows.
        DispatchQueue.main.async { update() }
    }

    /// Puts the Dock icon back before a window opens, so the window comes to front.
    static func showIcon() {
        set(.regular)
    }

    static func update() {
        set(!isOn || hasWindow ? .regular : .accessory)
    }

    private static var hasWindow: Bool {
        NSApp.windows.contains { w in
            !(w is NSPanel) && w.level == .normal && w.styleMask.contains(.titled)
                && (w.isVisible || w.isMiniaturized)
        }
    }

    private static func set(_ policy: NSApplication.ActivationPolicy) {
        guard NSApp.activationPolicy() != policy else { return }
        NSApp.setActivationPolicy(policy)
    }
}
