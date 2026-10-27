import Foundation
import HeraldoKit
import Observation
import os

let log = Logger(subsystem: Bundle.main.bundleIdentifier ?? "heraldo", category: "app")

/// The app's state: settings, the synced history, unread state and incidents.
/// Views read it; the platform push code calls `refresh()`.
@MainActor
@Observable
final class AppModel {
    static let shared = AppModel()

    private(set) var credentials: Credentials
    private(set) var cache: HistoryCache
    private(set) var unread = UnreadState(cursor: nil, ids: [])
    private(set) var incidents: [Event] = []
    private(set) var isSyncing = false
    private(set) var lastSync: Date?
    var lastError: String?
    /// An event to show, from a notification tap.
    var openEventID: String?

    /// Called with the badge after each refresh. The iOS app sets the icon badge.
    @ObservationIgnored var onBadge: ((Int) -> Void)?
    /// Called after the settings change, so push registration can run again.
    @ObservationIgnored var onCredentialsChanged: (() -> Void)?

    private let store = CredentialStore()
    @ObservationIgnored private var refreshTask: Task<Void, Never>?

    init() {
        #if DEBUG
        // Simulator runs: `SIMCTL_CHILD_HERALDO_URL=... xcrun simctl launch` seeds the
        // settings, and HERALDO_OPEN_EVENT opens one event.
        let env = ProcessInfo.processInfo.environment
        if let url = env["HERALDO_URL"], let read = env["HERALDO_READ_TOKEN"] {
            try? store.save(Credentials(serverURL: url, readToken: read, writeToken: env["HERALDO_WRITE_TOKEN"]))
        }
        #endif
        credentials = store.load()
        cache = Self.loadCache()
        #if DEBUG
        openEventID = env["HERALDO_OPEN_EVENT"]
        #endif
    }

    var isConfigured: Bool { credentials.isComplete }

    func client() throws -> HeraldoClient { try HeraldoClient(credentials: credentials) }

    func sync() throws -> HistorySync { HistorySync(client: try client()) }

    // MARK: Settings

    func save(_ c: Credentials) async throws -> Health {
        let trimmed = Credentials(serverURL: c.serverURL?.trimmingCharacters(in: .whitespacesAndNewlines),
                                  readToken: c.readToken?.trimmingCharacters(in: .whitespacesAndNewlines),
                                  writeToken: c.writeToken?.trimmingCharacters(in: .whitespacesAndNewlines))
        let probe = try HeraldoClient(credentials: trimmed)
        let health = try await probe.health()
        _ = try await probe.state() // proves the read token
        let changedServer = trimmed.serverURL != credentials.serverURL
        try store.save(trimmed)
        credentials = trimmed
        if changedServer {
            cache = HistoryCache()
            saveCache()
        }
        onCredentialsChanged?()
        await refresh()
        return health
    }

    // MARK: Sync

    /// Syncs history, the unread state and the open incidents. Calls made
    /// while one runs join it.
    func refresh() async {
        if let refreshTask { return await refreshTask.value }
        let task = Task { await self.doRefresh() }
        refreshTask = task
        await task.value
        refreshTask = nil
    }

    private func doRefresh() async {
        guard isConfigured else { return }
        isSyncing = true
        defer { isSyncing = false }
        do {
            let sync = try sync()
            cache = try await sync.sync(cache)
            saveCache()
            unread = try await sync.unread()
            incidents = try await sync.client.state().incidents
                .sorted { ($0.date ?? .distantPast) > ($1.date ?? .distantPast) }
            lastSync = .now
            lastError = nil
            onBadge?(unread.badge)
        } catch {
            log.error("refresh: \(error.localizedDescription, privacy: .public)")
            lastError = error.localizedDescription
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
        await run { try await $0.markRead(eventID: e.id) }
    }

    func markAllRead() async {
        await run { try await $0.markAllRead() }
    }

    private func run(_ body: (HistorySync) async throws -> ReadCursor?) async {
        do {
            let sync = try sync()
            _ = try await body(sync)
            unread = try await sync.unread()
            onBadge?(unread.badge)
        } catch {
            lastError = error.localizedDescription
        }
    }

    /// Acknowledges an incident with the write token.
    func ack(_ incident: Event) async throws {
        try await client().ack(incident, source: AppInfo.source)
        await refresh()
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
