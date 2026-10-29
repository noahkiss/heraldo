import Foundation
import HeraldoKit
import Network
import Observation
import SwiftUI
#if os(macOS)
import AppKit
#endif

/// Keeps the app current without polling, on both platforms. It owns the
/// HeraldoKit `SyncDriver` and feeds it: the network path, Low Power Mode,
/// the notification extension's signal and, through the platform code,
/// window visibility, sleep and the scene phase. The driver opens the event
/// stream, runs the safety net, and hands each sync to `AppModel.perform`.
@MainActor
final class AppSync {
    static let shared = AppSync()

    let stream = LiveStream()
    #if os(macOS)
    let net = MacSafetyNet()
    #else
    let net = NoSafetyNet()
    #endif
    private(set) var driver: SyncDriver!
    private var path: NWPathMonitor?
    private let pathQueue = DispatchQueue(label: "heraldo.sync.path", qos: .utility)
    private var observers: [NSObjectProtocol] = []
    private var signal: SyncSignal.Observer?
    /// After a wake: awake waits for the path monitor's fresh report.
    private var waking = false
    private var wakeFallback: Task<Void, Never>?
    private var started = false

    /// Starts the driver. `awake` is false on iOS until the scene is active.
    func start(awake: Bool) {
        guard !started else { return }
        started = true
        let model = AppModel.shared
        #if os(macOS)
        let config = SyncDriver.Config()
        #else
        // A glance at the app opens no stream.
        let config = SyncDriver.Config(streamDelay: 3)
        #endif
        let inputs = SyncInputs(configured: model.isConfigured, awake: awake,
                                lowPower: ProcessInfo.processInfo.isLowPowerModeEnabled)
        driver = SyncDriver(inputs: inputs, config: config, clock: SystemSyncClock(), stream: stream,
                            safetyNet: net) { r in
            await AppModel.shared.perform(r)
        }
        driver.onUILive = { UILiveness.shared.windows = $0 }
        UILiveness.shared.windows = driver.mode.uiLive
        stream.onMessage = { [weak self] m in self?.driver.receive(m) }
        startPathMonitor()
        observers.append(NotificationCenter.default.addObserver(
            forName: .NSProcessInfoPowerStateDidChange, object: nil, queue: .main
        ) { _ in
            MainActor.assumeIsolated {
                AppSync.shared.driver.update { $0.lowPower = ProcessInfo.processInfo.isLowPowerModeEnabled }
            }
        })
        if let id = Bundle.main.bundleIdentifier {
            signal = SyncSignal.observe(SyncSignal.name(appBundleID: id)) {
                MainActor.assumeIsolated { AppSync.shared.driver.hint() }
            }
        }
        log.info("sync: started, stream \(self.driver.mode.heartbeat.map { "\($0) s" } ?? "off", privacy: .public)")
        driver.launched()
    }

    /// The settings changed: the stream reconnects with the new server or
    /// token, and the next sync starts from a full refresh.
    func credentialsChanged() {
        guard started else { return }
        driver.update { $0.configured = AppModel.shared.isConfigured }
        stream.restart()
    }

    /// Sync now.
    func syncNow() async {
        guard started else { return await AppModel.shared.refresh() }
        await driver.syncNow()
    }

    // MARK: iOS scene

    /// The scene is active: the stream opens after a few seconds, and the
    /// vector is checked now (unless checked in the last 30 s).
    func sceneActive() {
        guard started else { return }
        driver.update {
            $0.awake = true
            $0.windowVisible = true
        }
        driver.opened()
    }

    /// The scene went to the background: the stream closes. Nothing runs
    /// until the next activation; the notification extension updates the
    /// widgets meanwhile.
    func sceneBackground() {
        guard started else { return }
        driver.update {
            $0.awake = false
            $0.windowVisible = false
        }
    }

    // MARK: Sleep and the network path

    func willSleep() {
        guard started else { return }
        wakeFallback?.cancel()
        waking = false
        driver.update { $0.awake = false }
    }

    /// Back from sleep: the stream and checks wait for the path monitor's
    /// fresh report, which comes once the network is back. A report that
    /// never comes is covered after 15 s.
    func didWake(then: @escaping @MainActor () -> Void = {}) {
        guard started else { return }
        waking = true
        wakeThen = then
        wakeFallback?.cancel()
        wakeFallback = Task { @MainActor in
            try? await Task.sleep(for: .seconds(15))
            guard !Task.isCancelled, self.waking else { return }
            self.waking = false
            self.driver.update { $0.awake = true }
            self.wakeThen?()
            self.wakeThen = nil
        }
        // A new monitor reports the path as it is now.
        startPathMonitor()
    }

    /// Runs once the wake completes.
    private var wakeThen: (@MainActor () -> Void)?

    private func startPathMonitor() {
        path?.cancel()
        let monitor = NWPathMonitor()
        monitor.pathUpdateHandler = { p in
            let network: NetworkPath = p.status == .satisfied ? .satisfied : .unsatisfied
            let expensive = p.isExpensive
            let constrained = p.isConstrained
            Task { @MainActor in AppSync.shared.pathChanged(network, expensive: expensive, constrained: constrained) }
        }
        monitor.start(queue: pathQueue)
        path = monitor
    }

    private func pathChanged(_ network: NetworkPath, expensive: Bool, constrained: Bool) {
        // Right after a wake the path can report up before it is: wait for up.
        let wake = waking && network == .satisfied
        if wake {
            waking = false
            wakeFallback?.cancel()
        }
        driver.update {
            $0.network = network
            $0.expensive = expensive
            $0.constrained = constrained
            if wake { $0.awake = true }
        }
        if wake {
            wakeThen?()
            wakeThen = nil
        }
    }
}

/// The event stream behind `SyncStreamControl`: one `EventStream` with the
/// current settings.
@MainActor
final class LiveStream: SyncStreamControl {
    var onMessage: ((StreamMessage) -> Void)?
    private var stream: EventStream?
    private var heartbeat: Int?

    func start(heartbeat: Int) {
        self.heartbeat = heartbeat
        stream?.stop()
        guard let client = try? AppModel.shared.client() else { return }
        let s = EventStream(client: client, heartbeat: heartbeat, onMessage: { m in
            Task { @MainActor in AppSync.shared.stream.onMessage?(m) }
        }, onStatus: { status in
            if case .failed(let wait) = status {
                log.info("sync: stream closed; again in \(Int(wait), privacy: .public) s")
            }
        })
        stream = s
        s.start()
        log.info("sync: stream on, heartbeat \(heartbeat, privacy: .public) s")
    }

    func stop() {
        stream?.stop()
        stream = nil
        heartbeat = nil
        log.info("sync: stream off")
    }

    /// Reconnects an open stream, as after the settings changed.
    func restart() {
        guard let heartbeat else { return }
        start(heartbeat: heartbeat)
    }
}

/// iOS runs no safety net: the scene's activation covers it.
@MainActor
final class NoSafetyNet: SafetyNetControl {
    func schedule(_ net: SafetyNet) {}
    func cancel() {}
}

#if os(macOS)
/// The safety net on the Mac: `NSBackgroundActivityScheduler`, so the system
/// picks a moment it is awake anyway. Each run is one revisions check.
@MainActor
final class MacSafetyNet: SafetyNetControl {
    private var scheduler: NSBackgroundActivityScheduler?

    func schedule(_ net: SafetyNet) {
        cancel()
        let s = NSBackgroundActivityScheduler(identifier: (Bundle.main.bundleIdentifier ?? "heraldo") + ".safety-net")
        s.repeats = true
        s.interval = net.interval
        s.tolerance = net.tolerance
        s.qualityOfService = .utility
        s.schedule { done in
            Task { @MainActor in
                await AppSync.shared.driver.safetyNetFired()
                done(.finished)
            }
        }
        scheduler = s
        log.info("sync: safety net every \(Int(net.interval / 60), privacy: .public) min")
    }

    func cancel() {
        scheduler?.invalidate()
        scheduler = nil
    }
}
#endif

/// Whether the UI is on screen, for relative times and animations.
@MainActor
@Observable
final class UILiveness {
    static let shared = UILiveness()
    /// A window is visible (the driver's `uiLive`).
    var windows = true
    /// The menu bar panel is open.
    var menu = false
}

/// A periodic schedule that stops while paused: one entry, then none.
struct PausableSchedule: TimelineSchedule {
    var interval: TimeInterval
    var paused: Bool

    func entries(from start: Date, mode: TimelineScheduleMode) -> AnyIterator<Date> {
        var next: Date? = start
        let step = interval
        let once = paused
        return AnyIterator {
            guard let current = next else { return nil }
            next = once ? nil : current.addingTimeInterval(step)
            return current
        }
    }
}
