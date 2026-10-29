import Foundation
import HeraldoKit
import Observation
import os

let log = Logger(subsystem: Bundle.main.bundleIdentifier ?? "heraldo", category: "app")

/// The app's state: settings, the synced history, unread state and incidents.
/// Views read it. `AppSync` drives `perform(_:)`; Sync now calls `refresh()`.
@MainActor
@Observable
final class AppModel {
    static let shared = AppModel()

    private(set) var credentials: Credentials
    private(set) var cache: HistoryCache
    private(set) var unread = UnreadState(cursor: nil, ids: [])
    private(set) var incidents: [Event] = []
    /// Every check, in key order. Empty on a server before check-ins.
    private(set) var checks: [Check] = []
    /// The projects the server's events name, from `GET /v1/projects`.
    /// Empty on a server before projects.
    private(set) var projects: [String] = []
    /// The saved views, by position then name, from `GET /v1/views`.
    private(set) var views: [SavedView] = []
    /// The server answers `/v1/views`. Off on a server before views: the
    /// Views UI hides.
    private(set) var viewsSupported = false
    /// The server's assistant takes questions (`GET /v1/llm/status` chat).
    /// Off on a server before 0.8: the Ask buttons hide.
    private(set) var chatAvailable = false
    /// The running Live Activities. The Mac app's home; empty on iOS and
    /// on a server before Live Activities.
    private(set) var activities: [ActivitySummary] = []
    /// The hold's open entries. The Mac app's home; empty on iOS and on a
    /// server before the hold.
    private(set) var held: [HoldEntry] = []
    private(set) var isSyncing = false
    private(set) var lastSync: Date?
    var lastError: String?
    /// An event to show, from a notification tap.
    var openEventID: String?

    /// Called with the badge after each refresh. The iOS app sets the icon badge.
    @ObservationIgnored var onBadge: ((Int) -> Void)?
    /// Called after the settings change, so push registration can run again.
    @ObservationIgnored var onCredentialsChanged: (() -> Void)?
    /// Brings the history window up. The Mac app sets it; iOS has one window.
    @ObservationIgnored var showMain: (() -> Void)?

    let store = CredentialStore()
    /// The vector the cache and state were last brought up to. In memory
    /// only: a launch starts with one full refresh.
    @ObservationIgnored private var applied: Revisions?
    /// Whether the server answers `/v1/revisions`; nil until asked.
    @ObservationIgnored private var hasRevisions: Bool?
    /// The requests waiting for the running sync, merged.
    @ObservationIgnored private var queued: SyncRequest?
    @ObservationIgnored private var syncTask: Task<Void, Never>?
    @ObservationIgnored private var sentBadge: Int?

    init() {
        // The first builds kept device-only items; move them to the synced one.
        if store.migrateLegacy() { log.info("credentials: moved to the synced Keychain item") }
        #if DEBUG
        // Simulator runs: `SIMCTL_CHILD_HERALDO_URL=... xcrun simctl launch` seeds the
        // settings, and HERALDO_OPEN_EVENT opens one event.
        let env = ProcessInfo.processInfo.environment
        if let url = env["HERALDO_URL"], let read = env["HERALDO_READ_TOKEN"] {
            try? store.save(Credentials(serverURL: url, readToken: read, writeToken: env["HERALDO_WRITE_TOKEN"]))
        }
        #endif
        var loadError: Error?
        do {
            credentials = try store.read() ?? Credentials()
        } catch {
            credentials = Credentials()
            loadError = error
        }
        cache = Self.loadCache()
        if let loadError {
            lastError = loadError.localizedDescription
            log.error("credentials: \(loadError.localizedDescription, privacy: .public)")
        } else {
            log.info("credentials: \(self.isConfigured ? "found" : "none", privacy: .public) in \(self.store.modeDescription, privacy: .public)")
        }
        #if DEBUG
        openEventID = env["HERALDO_OPEN_EVENT"]
        #endif
    }

    var isConfigured: Bool { credentials.isComplete }

    /// Open incidents not yet acknowledged.
    var unackedIncidents: Int { incidents.filter { !$0.isAcked }.count }

    /// Checks that are down, the longest down first.
    var down: [Check] {
        checks.filter { $0.state == .down }.sorted { ($0.stateSince ?? .distantPast) < ($1.stateSince ?? .distantPast) }
    }

    /// The number of checks that are down: the Check-ins badge.
    var downChecks: Int { down.count }

    /// Shows one event, from a notification or a heraldo:// link.
    func openEvent(_ id: String) {
        openEventID = id
        showMain?()
    }

    func client() throws -> HeraldoClient { try HeraldoClient(credentials: credentials) }

    func sync() throws -> HistorySync { HistorySync(client: try client()) }

    // MARK: Settings

    func save(_ c: Credentials) async throws -> Health {
        let trimmed = Credentials(serverURL: c.serverURL?.trimmingCharacters(in: .whitespacesAndNewlines),
                                  readToken: c.readToken?.trimmingCharacters(in: .whitespacesAndNewlines),
                                  writeToken: c.writeToken?.trimmingCharacters(in: .whitespacesAndNewlines),
                                  adminToken: c.adminToken?.trimmingCharacters(in: .whitespacesAndNewlines))
        let probe = try HeraldoClient(credentials: trimmed)
        let health = try await probe.health()
        _ = try await probe.state() // proves the read token
        let changedServer = trimmed.serverURL != credentials.serverURL
        try store.save(trimmed)
        credentials = trimmed
        applied = nil
        hasRevisions = nil
        if changedServer {
            cache = HistoryCache()
            saveCache()
        }
        onCredentialsChanged?()
        await refresh()
        return health
    }

    // MARK: Sync

    /// Sync now: everything, whatever the vector says.
    func refresh() async {
        await perform(.fullRefresh)
    }

    /// Fetches these parts, after the app's own write.
    func refresh(_ parts: Set<SyncPart>) async {
        await perform(SyncRequest(parts: parts))
    }

    /// Runs a sync request. One runs at a time; a request made meanwhile
    /// joins the queue, and the call returns once it ran.
    func perform(_ r: SyncRequest) async {
        if queued == nil { queued = r } else { queued?.merge(r) }
        if let syncTask { return await syncTask.value }
        let task = Task { @MainActor in
            while let next = self.queued {
                self.queued = nil
                await self.run(next)
            }
            // No suspension between the last look at the queue and here,
            // so a request queued now starts a new task.
            self.syncTask = nil
        }
        syncTask = task
        await task.value
    }

    /// The client for syncs nobody asked for: it waits for the network
    /// instead of failing at once, as after a wake.
    private static let waitingSession: URLSession = {
        let c = URLSessionConfiguration.default
        c.waitsForConnectivity = true
        c.timeoutIntervalForResource = 90
        return URLSession(configuration: c)
    }()

    private func run(_ r: SyncRequest) async {
        guard isConfigured else { return }
        let client: HeraldoClient
        do {
            client = try HeraldoClient(credentials: credentials, session: r.full ? .shared : Self.waitingSession)
        } catch {
            lastError = error.localizedDescription
            return
        }
        var vector = r.vector
        if vector != nil { hasRevisions = true }
        // Learn the vector before fetching, so a change made during the
        // fetch shows up as a change next time.
        if (r.check || r.full), vector == nil, hasRevisions != false {
            do {
                vector = try await client.revisions()
                hasRevisions = vector != nil
            } catch {
                if !r.full {
                    log.error("revisions: \(error.localizedDescription, privacy: .public)")
                    lastError = error.localizedDescription
                    return
                }
            }
        }
        let plan: SyncPlan
        if r.full {
            plan = .full
        } else if let vector {
            plan = vector.plan(since: applied, poke: r.poke).union(r.parts)
        } else if r.poke || r.check {
            // A server before revisions: the old full refresh.
            plan = .full
        } else {
            plan = .parts(r.parts)
        }
        let parts: Set<SyncPart>
        switch plan {
        case .full: parts = Set(SyncPart.allCases)
        case .parts(let p): parts = p
        }
        if parts.isEmpty {
            if let vector { applied = vector }
            return
        }
        if await fetch(parts, client), let vector { applied = vector }
    }

    /// Fetches the parts. Each value is set only when it changed: a set
    /// redraws every view that reads it, and most syncs bring little.
    private func fetch(_ parts: Set<SyncPart>, _ client: HeraldoClient) async -> Bool {
        isSyncing = true
        defer { isSyncing = false }
        let sync = HistorySync(client: client)
        do {
            if parts.contains(.history) {
                let synced = try await sync.sync(cache)
                if synced != cache {
                    cache = synced
                    saveCache()
                }
            }
            if parts.contains(.unread) {
                update(\.unread, try await sync.unread())
                if sentBadge != unread.badge {
                    sentBadge = unread.badge
                    onBadge?(unread.badge)
                }
            }
            // Open incidents before acknowledged ones, newest first in each.
            if parts.contains(.state) { update(\.incidents, try await client.state().sortedIncidents) }
            if parts.contains(.checks) { update(\.checks, try await client.checks() ?? []) }
            if parts.contains(.projects) {
                update(\.projects, (try? await client.projects())?.projects.map(\.name) ?? [])
            }
            if parts.contains(.views) { await reloadViews(client) }
            if parts.contains(.llm) { update(\.chatAvailable, (try? await client.llmStatus())?.chat ?? false) }
            #if os(macOS)
            if parts.contains(.activities) { update(\.activities, (try? await client.activities()) ?? []) }
            if parts.contains(.held) {
                update(\.held, (try? await client.heldEntries())?.held.filter(\.isOpen) ?? [])
            }
            #endif
            lastSync = .now
            if lastError != nil { lastError = nil }
            if !parts.isDisjoint(with: [.history, .state, .checks, .projects]) { Widgets.reload(self) }
            return true
        } catch {
            log.error("sync: \(error.localizedDescription, privacy: .public)")
            lastError = error.localizedDescription
            return false
        }
    }

    func loadOlder() async {
        guard let sync = try? sync() else { return }
        do {
            cache = try await sync.loadOlder(cache)
            saveCache()
        } catch {
            lastError = error.localizedDescription
        }
    }

    func isUnread(_ e: Event) -> Bool { unread.ids.contains(e.id) }

    func markRead(_ e: Event) async {
        await run { try await $0.markRead(e) }
    }

    func markAllRead() async {
        await run { try await $0.markAllRead() }
    }

    private func run(_ body: (HistorySync) async throws -> ReadCursor?) async {
        do {
            let sync = try sync()
            _ = try await body(sync)
            unread = try await sync.unread()
            sentBadge = unread.badge
            onBadge?(unread.badge)
        } catch {
            lastError = error.localizedDescription
        }
    }

    /// Acknowledges an incident with the write token.
    func ack(_ incident: Event) async throws {
        try await client().ack(incident, source: AppInfo.source)
        await refresh([.state, .history, .unread])
    }

    /// The projects for the Inbox's project filter: the server's list and
    /// the ones in the cache.
    var projectChoices: [String] {
        Array(Set(projects).union(cache.projects))
            .sorted { $0.localizedCaseInsensitiveCompare($1) == .orderedAscending }
    }

    /// Whether the write token is set: the write actions apply.
    var canWrite: Bool { !(credentials.writeToken?.isEmpty ?? true) }

    /// Mutes every event of a project for an hour, with the write token.
    func muteProject(_ project: String) async throws {
        let m = try await client().mute(MuteInput(project: project, for: "1h"))
        log.info("muted project \(project, privacy: .public) until \(m.until, privacy: .public)")
    }

    /// Pauses or resumes a check with the write token.
    func setPaused(_ check: Check, _ paused: Bool) async throws {
        try await client().setPaused(check.key, paused)
        await refresh([.checks, .state])
    }

    /// Fetches the saved views. A failed fetch keeps the ones shown.
    func reloadViews(_ with: HeraldoClient? = nil) async {
        let list: [SavedView]?
        do { list = try await (with ?? client()).views() } catch { return }
        update(\.views, list ?? [])
        update(\.viewsSupported, list != nil)
    }

    /// Sets a property only when the value differs.
    private func update<T: Equatable>(_ key: ReferenceWritableKeyPath<AppModel, T>, _ value: T) {
        if self[keyPath: key] != value { self[keyPath: key] = value }
    }

    // MARK: Cache file

    private static var cacheURL: URL {
        let dir = URL.applicationSupportDirectory
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        return dir.appending(path: "history.json")
    }

    private static func loadCache() -> HistoryCache {
        guard let data = try? Data(contentsOf: cacheURL),
              let c = try? JSON.decoder.decode(HistoryCache.self, from: data) else { return HistoryCache() }
        return c
    }

    private func saveCache() {
        do {
            try JSON.encoder.encode(cache).write(to: Self.cacheURL, options: .atomic)
        } catch {
            log.error("cache: \(error.localizedDescription, privacy: .public)")
        }
    }
}
