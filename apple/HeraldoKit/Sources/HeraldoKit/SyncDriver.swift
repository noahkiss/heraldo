import Foundation

/// What a sync must do. Requests merge: the debounce collects every trigger
/// of a burst into one.
public struct SyncRequest: Equatable, Sendable {
    /// Fetch everything: Sync now, first launch, settings saved.
    public var full = false
    /// The newest vector the stream brought.
    public var vector: Revisions?
    /// A poke arrived: domains the vector lacks are fetched too, and a poke
    /// with no vector (a server before revisions) means a full refresh.
    public var poke = false
    /// Ask `GET /v1/revisions` first.
    public var check = false
    /// Parts to fetch whatever the vector says, after the app's own writes.
    public var parts: Set<SyncPart> = []

    public init(full: Bool = false, vector: Revisions? = nil, poke: Bool = false, check: Bool = false,
                parts: Set<SyncPart> = []) {
        self.full = full
        self.vector = vector
        self.poke = poke
        self.check = check
        self.parts = parts
    }

    public static let fullRefresh = SyncRequest(full: true)
    public static let revisionsCheck = SyncRequest(check: true)

    public var isEmpty: Bool { !full && vector == nil && !poke && !check && parts.isEmpty }

    /// This request and a later one, as one.
    public mutating func merge(_ later: SyncRequest) {
        full = full || later.full
        if let v = later.vector { vector = v }
        poke = poke || later.poke
        check = check || later.check
        parts.formUnion(later.parts)
    }
}

/// The network path, as `NWPathMonitor` reports it.
public enum NetworkPath: Equatable, Sendable {
    /// No report yet: treated as up.
    case unknown
    case satisfied
    case unsatisfied
}

/// What the sync driver watches.
public struct SyncInputs: Equatable, Sendable {
    /// The server URL and read token are set.
    public var configured = false
    /// The app may work: false while the Mac sleeps, or while the iOS scene
    /// is not active.
    public var awake = true
    /// A window is on screen: on the Mac a visible window that is not
    /// occluded (the menu bar panel does not count); on iOS the active scene.
    public var windowVisible = false
    public var lowPower = false
    public var network: NetworkPath = .unknown
    /// The path is expensive (a hotspot, cellular) or constrained (Low Data Mode).
    public var expensive = false
    public var constrained = false

    public init(configured: Bool = false, awake: Bool = true, windowVisible: Bool = false, lowPower: Bool = false,
                network: NetworkPath = .unknown, expensive: Bool = false, constrained: Bool = false) {
        self.configured = configured
        self.awake = awake
        self.windowVisible = windowVisible
        self.lowPower = lowPower
        self.network = network
        self.expensive = expensive
        self.constrained = constrained
    }

    /// Low Power Mode or a costly path: discretionary work waits.
    public var saving: Bool { lowPower || expensive || constrained }
}

/// The safety net's cadence, in seconds.
public struct SafetyNet: Equatable, Sendable {
    public var interval: TimeInterval
    public var tolerance: TimeInterval

    public init(interval: TimeInterval, tolerance: TimeInterval) {
        self.interval = interval
        self.tolerance = tolerance
    }

    public static let normal = SafetyNet(interval: 15 * 60, tolerance: 5 * 60)
    public static let saving = SafetyNet(interval: 30 * 60, tolerance: 10 * 60)
}

/// What the driver runs for a set of inputs.
public struct SyncMode: Equatable, Sendable {
    /// The stream's heartbeat in seconds; nil keeps the stream closed.
    public var heartbeat: Int?
    /// The safety net's cadence; nil runs none.
    public var safetyNet: SafetyNet?
    /// Relative times and animations run. Off with no window on screen.
    public var uiLive: Bool

    public init(heartbeat: Int?, safetyNet: SafetyNet?, uiLive: Bool) {
        self.heartbeat = heartbeat
        self.safetyNet = safetyNet
        self.uiLive = uiLive
    }

    /// The heartbeat with a window on screen, and without.
    public static let visibleHeartbeat = 25
    public static let backgroundHeartbeat = 60

    /// The state table (battery design, section 5.2):
    ///
    /// | Inputs | Stream | Safety net |
    /// |---|---|---|
    /// | not configured, asleep, or no network | closed | none |
    /// | window visible | 25 s heartbeat | 15 min |
    /// | menu bar only | 60 s heartbeat | 15 min |
    /// | Low Power or a costly path, window visible | 25 s | 30 min |
    /// | Low Power or a costly path, no window | closed | 30 min |
    public static func of(_ i: SyncInputs) -> SyncMode {
        let ui = i.awake && i.windowVisible
        guard i.configured, i.awake, i.network != .unsatisfied else {
            return SyncMode(heartbeat: nil, safetyNet: nil, uiLive: ui)
        }
        let heartbeat: Int? = i.windowVisible ? visibleHeartbeat : (i.saving ? nil : backgroundHeartbeat)
        return SyncMode(heartbeat: heartbeat, safetyNet: i.saving ? .saving : .normal, uiLive: ui)
    }
}

/// Opens and closes the event stream. The stream sends its messages to
/// `SyncDriver.receive(_:)`.
@MainActor
public protocol SyncStreamControl: AnyObject {
    func start(heartbeat: Int)
    func stop()
}

/// Runs the safety net: a revisions check on a slow, system-chosen cadence.
@MainActor
public protocol SafetyNetControl: AnyObject {
    func schedule(_ net: SafetyNet)
    func cancel()
}

/// One pending timer.
@MainActor
public protocol SyncTimer: AnyObject {
    func cancel()
}

/// The clock: now, and one-shot timers. Tests pass a manual one.
@MainActor
public protocol SyncClock: AnyObject {
    var now: Date { get }
    func after(_ seconds: TimeInterval, _ body: @escaping @MainActor () -> Void) -> SyncTimer
}

/// The live clock: `Task.sleep` with a tenth of the delay as tolerance.
@MainActor
public final class SystemSyncClock: SyncClock {
    public init() {}
    public var now: Date { Date() }

    public func after(_ seconds: TimeInterval, _ body: @escaping @MainActor () -> Void) -> SyncTimer {
        let t = SleepTimer()
        t.task = Task { @MainActor in
            try? await Task.sleep(for: .seconds(seconds), tolerance: .seconds(max(seconds / 10, 0.1)))
            if !Task.isCancelled { body() }
        }
        return t
    }

    final class SleepTimer: SyncTimer {
        var task: Task<Void, Never>?
        func cancel() { task?.cancel() }
    }
}

/// Keeps the app current without polling. Every trigger (a stream poke, a
/// push, a wake, a window opening, the safety net) becomes one
/// `SyncRequest`; a short debounce merges a burst into one sync. The
/// inputs decide whether the stream is open and how often the safety net
/// runs (`SyncMode.of`).
@MainActor
public final class SyncDriver {
    public struct Config: Sendable {
        /// How long a request waits for more to join it.
        public var debounce: TimeInterval = 1.5
        /// A window or the menu opening checks the vector when the last
        /// check is older than this.
        public var checkMaxAge: TimeInterval = 30
        /// How long the inputs must ask for the stream before it opens:
        /// iOS waits a few seconds so a glance at the app opens nothing.
        public var streamDelay: TimeInterval = 0

        public init(debounce: TimeInterval = 1.5, checkMaxAge: TimeInterval = 30, streamDelay: TimeInterval = 0) {
            self.debounce = debounce
            self.checkMaxAge = checkMaxAge
            self.streamDelay = streamDelay
        }
    }

    public private(set) var inputs: SyncInputs
    public private(set) var mode: SyncMode
    /// The heartbeat of the open stream; nil while it is closed.
    public private(set) var streamHeartbeat: Int?
    /// The safety net in force.
    public private(set) var safetyNet: SafetyNet?
    /// When the app last learned the server's vector, or last checked.
    public private(set) var lastCheck: Date?
    /// Called when `mode.uiLive` changes.
    public var onUILive: ((Bool) -> Void)?

    private let config: Config
    private let clock: SyncClock
    private let stream: SyncStreamControl
    private let net: SafetyNetControl
    private let perform: @MainActor (SyncRequest) async -> Void
    private var pending: SyncRequest?
    /// When the earliest pending check was asked for.
    private var pendingCheckSince: Date?
    private var debounceTimer: SyncTimer?
    private var streamTimer: SyncTimer?

    public init(inputs: SyncInputs = SyncInputs(), config: Config = Config(), clock: SyncClock,
                stream: SyncStreamControl, safetyNet: SafetyNetControl,
                perform: @escaping @MainActor (SyncRequest) async -> Void) {
        self.inputs = inputs
        self.config = config
        self.clock = clock
        self.stream = stream
        self.net = safetyNet
        self.perform = perform
        mode = SyncMode.of(inputs)
        apply(from: nil)
    }

    // MARK: Inputs

    /// Changes the inputs and applies the mode they give.
    public func update(_ change: (inout SyncInputs) -> Void) {
        let old = inputs
        change(&inputs)
        guard inputs != old else { return }
        mode = SyncMode.of(inputs)
        apply(from: old)
    }

    private func apply(from old: SyncInputs?) {
        // The stream: open or close. A heartbeat change alone waits for the
        // next connect; reconnecting for it would cost more than it saves.
        if let beat = mode.heartbeat {
            if streamHeartbeat == nil && streamTimer == nil {
                if config.streamDelay > 0 {
                    streamTimer = clock.after(config.streamDelay) { [weak self] in
                        guard let self else { return }
                        self.streamTimer = nil
                        if let b = self.mode.heartbeat, self.streamHeartbeat == nil { self.openStream(b) }
                    }
                } else {
                    openStream(beat)
                }
            }
        } else {
            streamTimer?.cancel()
            streamTimer = nil
            if streamHeartbeat != nil {
                streamHeartbeat = nil
                stream.stop()
            }
        }
        // The safety net.
        if mode.safetyNet != safetyNet {
            safetyNet = mode.safetyNet
            if let n = safetyNet { net.schedule(n) } else { net.cancel() }
        }
        guard let old else {
            onUILive?(mode.uiLive)
            return
        }
        if SyncMode.of(old).uiLive != mode.uiLive { onUILive?(mode.uiLive) }
        if inputs.configured && !old.configured {
            request(.fullRefresh)
            return
        }
        // Back from sleep, or the network returned: an open stream sends its
        // vector on connect; with none, ask once.
        let woke = inputs.awake && !old.awake
        let netBack = inputs.network != .unsatisfied && old.network == .unsatisfied
        if (woke || netBack) && inputs.configured && inputs.awake && inputs.network != .unsatisfied
            && mode.heartbeat == nil {
            requestCheck()
        }
    }

    private func openStream(_ beat: Int) {
        streamHeartbeat = beat
        stream.start(heartbeat: beat)
    }

    // MARK: Triggers

    /// A message from the stream.
    public func receive(_ message: StreamMessage) {
        switch message {
        case .revisions(let v):
            lastCheck = clock.now
            request(SyncRequest(vector: v))
        case .poke(let p):
            if p.revisions != nil { lastCheck = clock.now }
            request(SyncRequest(vector: p.revisions, poke: true))
        case .keepAlive:
            break
        }
    }

    /// The app launched: learn the vector. The first one applied means a
    /// full refresh. An open stream's first message answers it.
    public func launched() {
        guard inputs.configured else { return }
        requestCheck()
    }

    /// Something says the server changed: a push, the notification
    /// extension, a notification action. The vector is checked unless the
    /// stream brings one before the debounce ends.
    public func hint() {
        requestCheck()
    }

    /// A window or the menu opened, or the app became active: check the
    /// vector when the last check is old.
    public func opened() {
        guard inputs.configured else { return }
        if let last = lastCheck, clock.now.timeIntervalSince(last) < config.checkMaxAge { return }
        requestCheck()
    }

    /// The safety net fired: one revisions check, now.
    public func safetyNetFired() async {
        guard inputs.configured, inputs.awake else { return }
        lastCheck = clock.now
        await perform(.revisionsCheck)
    }

    /// Sync now: everything, at once.
    public func syncNow() async {
        lastCheck = clock.now
        await perform(.fullRefresh)
    }

    /// A request through the debounce.
    public func request(_ r: SyncRequest) {
        guard !r.isEmpty else { return }
        if pending == nil { pending = r } else { pending?.merge(r) }
        guard debounceTimer == nil else { return }
        debounceTimer = clock.after(config.debounce) { [weak self] in self?.flush() }
    }

    private func requestCheck() {
        if pendingCheckSince == nil { pendingCheckSince = clock.now }
        request(.revisionsCheck)
    }

    private func flush() {
        debounceTimer = nil
        guard var r = pending else { return }
        pending = nil
        // A vector that came after the check was asked for answers it.
        if r.check, let since = pendingCheckSince, let last = lastCheck, last >= since, !r.full {
            r.check = false
        }
        pendingCheckSince = nil
        guard !r.isEmpty else { return }
        if r.check || r.full { lastCheck = clock.now }
        Task { @MainActor in await self.perform(r) }
    }
}
