import AppKit
import HeraldoKit

/// The Mac inputs of `AppSync`: whether a window is on screen (occlusion),
/// the menu bar panel, sleep and wake. A window or the menu opening asks
/// for a revisions check when the last one is older than 30 s.
@MainActor
enum MacSync {
    static func start() {
        AppSync.shared.start(awake: true)
        let center = NotificationCenter.default
        for name in [NSWindow.didChangeOcclusionStateNotification, NSWindow.didBecomeKeyNotification,
                     NSWindow.didMiniaturizeNotification, NSWindow.didDeminiaturizeNotification,
                     NSApplication.didChangeOcclusionStateNotification] {
            center.addObserver(forName: name, object: nil, queue: .main) { _ in
                MainActor.assumeIsolated { update() }
            }
        }
        // The window is still on screen during willClose; look once it is gone.
        center.addObserver(forName: NSWindow.willCloseNotification, object: nil, queue: .main) { _ in
            DispatchQueue.main.async { update() }
        }
        let workspace = NSWorkspace.shared.notificationCenter
        workspace.addObserver(forName: NSWorkspace.willSleepNotification, object: nil, queue: .main) { _ in
            MainActor.assumeIsolated { AppSync.shared.willSleep() }
        }
        workspace.addObserver(forName: NSWorkspace.didWakeNotification, object: nil, queue: .main) { _ in
            MainActor.assumeIsolated {
                AppSync.shared.didWake { Task { await PushManager.shared.foreground() } }
            }
        }
        DispatchQueue.main.async { update() }
    }

    private static var lastWindows = false
    private static var lastMenu = false

    static func update() {
        let windows = NSApp.windows.contains { w in
            isMainKind(w) && w.isVisible && !w.isMiniaturized && w.occlusionState.contains(.visible)
        }
        let menu = NSApp.windows.contains { w in
            isMenuPanel(w) && w.isVisible && w.occlusionState.contains(.visible)
        }
        let driver = AppSync.shared.driver
        if windows != lastWindows {
            lastWindows = windows
            driver?.update { $0.windowVisible = windows }
        }
        if menu != lastMenu {
            lastMenu = menu
            UILiveness.shared.menu = menu
        }
        if (windows && NSApp.isActive) || menu { driver?.opened() }
    }

    /// A titled window at the normal level: the main, detail and Settings
    /// windows, as DockPolicy counts them.
    private static func isMainKind(_ w: NSWindow) -> Bool {
        !(w is NSPanel) && w.level == .normal && w.styleMask.contains(.titled)
    }

    /// The menu bar item's panel. The status item's own window is not one.
    private static func isMenuPanel(_ w: NSWindow) -> Bool {
        w is NSPanel && !String(describing: type(of: w)).contains("StatusBar")
    }
}
