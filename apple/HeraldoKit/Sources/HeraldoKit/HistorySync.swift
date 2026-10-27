import Foundation

/// The history the app keeps on the device, newest first, and the cursors
/// that continue it. A push only says "something new"; the app syncs this
/// from the server by cursor on launch and on foreground.
public struct HistoryCache: Codable, Equatable, Sendable {
    /// Newest first.
    public var events: [Event] = []
    /// The cursor of the newest event synced: the next sync lists after it.
    public var syncCursor: String?
    /// The cursor of the oldest event kept: "load older" lists before it.
    public var olderCursor: String?
    /// The server has no events older than the oldest kept.
    public var reachedEnd = false

    /// The most events kept. Past it the oldest go, and with them the way to
    /// page further back from the cache.
    public static let limit = 3000

    public init() {}

    /// Adds events newer than every kept one. `newest` is newest first.
    mutating func prepend(_ newest: [Event]) {
        let known = Set(events.map(\.id))
        events.insert(contentsOf: newest.filter { !known.contains($0.id) }, at: 0)
        trim()
    }

    /// Adds events older than every kept one. `older` is newest first.
    mutating func append(_ older: [Event]) {
        let known = Set(events.map(\.id))
        events.append(contentsOf: older.filter { !known.contains($0.id) })
        trim()
    }

    private mutating func trim() {
        guard events.count > Self.limit else { return }
        events.removeLast(events.count - Self.limit)
        olderCursor = nil
        reachedEnd = true
    }

    /// The hold states seen in the cache, with `held` always first, for the hold filter.
    public var holdStates: [String] {
        let seen = Set(events.compactMap { $0.hold?.state }).subtracting(["held"])
        return ["held"] + seen.sorted()
    }

    /// Resolved incidents since `date`, newest first, one per key.
    public func resolvedIncidents(since date: Date) -> [Event] {
        var keys = Set<String>()
        return events.filter { e in
            guard e.kind == "incident", e.action == "resolve", let key = e.key,
                  (e.date ?? .distantPast) >= date, !keys.contains(key) else { return false }
            keys.insert(key)
            return true
        }
    }

    /// The sources seen in the cache, sorted, for the source filter.
    public var sources: [String] {
        Array(Set(events.map(\.source))).sorted { $0.localizedCaseInsensitiveCompare($1) == .orderedAscending }
    }
}

/// The unread state of one read cursor.
public struct UnreadState: Equatable, Sendable {
    /// The stored cursor, nil when no client has made it yet.
    public var cursor: ReadCursor?
    /// The ids of events after the cursor, as far as the app looked.
    public var ids: Set<String>

    public init(cursor: ReadCursor?, ids: Set<String>) {
        self.cursor = cursor
        self.ids = ids
    }

    /// The badge: the server's count of alerting events after the cursor.
    public var badge: Int { cursor?.unread ?? 0 }
}

/// History sync and the shared read cursor, over the API client.
public struct HistorySync: Sendable {
    /// The read cursor every client shares, the CLI's default.
    public static let inbox = "inbox"

    public var client: HeraldoClient
    /// Events per page.
    public var pageSize: Int
    /// The most pages one call reads before it stops and leaves the rest
    /// for the next call.
    public var maxPages: Int

    public init(client: HeraldoClient, pageSize: Int = 200, maxPages: Int = 10) {
        self.client = client
        self.pageSize = pageSize
        self.maxPages = maxPages
    }

    /// Brings the cache up to date. A fresh cache gets the newest page; a
    /// synced one gets every event after its cursor, oldest first, in pages.
    public func sync(_ cache: HistoryCache) async throws -> HistoryCache {
        var c = cache
        if c.syncCursor == nil {
            // The head first: an event stored between the two calls shows up
            // in both and the merge drops the copy, so nothing falls between.
            guard let head = try await client.headCursor() else { return c }
            let page = try await client.listEvents(EventQuery(order: .desc, limit: pageSize))
            c.prepend(page.events)
            c.olderCursor = page.nextCursor
            c.reachedEnd = page.events.count < pageSize
            c.syncCursor = head
        }
        for _ in 0..<maxPages {
            let page = try await client.listEvents(EventQuery(after: c.syncCursor, order: .asc, limit: pageSize))
            guard !page.events.isEmpty else { break }
            c.prepend(page.events.reversed())
            if let next = page.nextCursor { c.syncCursor = next }
            if page.events.count < pageSize { break }
        }
        return c
    }

    /// Adds the page before the oldest kept event.
    public func loadOlder(_ cache: HistoryCache) async throws -> HistoryCache {
        var c = cache
        guard !c.reachedEnd, let before = c.olderCursor else { return c }
        let page = try await client.listEvents(EventQuery(before: before, order: .desc, limit: pageSize))
        c.append(page.events)
        if let next = page.nextCursor { c.olderCursor = next }
        c.reachedEnd = page.events.count < pageSize
        return c
    }

    /// A server-side search or filter, newest first. Not cached.
    public func search(q: String?, kind: String?, source: String?, hold: String? = nil,
                       before: String? = nil) async throws -> EventPage {
        try await client.listEvents(EventQuery(before: before, order: .desc, limit: pageSize,
                                               source: source, kind: kind, q: q, hold: hold))
    }

    /// The read cursor and the events after it, up to `maxPages` pages.
    public func unread(_ name: String = HistorySync.inbox) async throws -> UnreadState {
        guard let cursor = try await client.cursor(name) else {
            return UnreadState(cursor: nil, ids: [])
        }
        var ids = Set<String>()
        var after = cursor.cursor
        for _ in 0..<maxPages {
            let page = try await client.listEvents(EventQuery(after: after, order: .asc, limit: pageSize))
            ids.formUnion(page.events.map(\.id))
            guard page.events.count == pageSize, let next = page.nextCursor else { break }
            after = next
        }
        return UnreadState(cursor: cursor, ids: ids)
    }

    /// Marks everything read: moves the cursor to the newest event.
    @discardableResult
    public func markAllRead(_ name: String = HistorySync.inbox) async throws -> ReadCursor? {
        guard let head = try await client.headCursor() else { return nil }
        return try await client.moveCursor(name, to: head)
    }

    /// Marks one event read. One cursor is one high-water mark, so this marks
    /// every event up to it read too, and never moves the cursor back: the
    /// server keeps a cursor that is already past the event.
    @discardableResult
    public func markRead(eventID: String, _ name: String = HistorySync.inbox) async throws -> ReadCursor? {
        guard let cursor = try await cursorOf(eventID: eventID) else { return nil }
        return try await client.moveCursor(name, to: cursor)
    }

    /// Marks one event read, by its own cursor when the server sent one: one
    /// call. Without it, markRead(eventID:) looks the cursor up.
    @discardableResult
    public func markRead(_ event: Event, _ name: String = HistorySync.inbox) async throws -> ReadCursor? {
        guard let cursor = event.cursor else { return try await markRead(eventID: event.id, name) }
        return try await client.moveCursor(name, to: cursor)
    }

    /// The list cursor of one event. An event does not carry its cursor; a
    /// page does, as the cursor of its last event. This finds the event in
    /// the newest pages, then asks for the page that ends on it.
    public func cursorOf(eventID: String) async throws -> String? {
        attempt: for _ in 0..<2 {
            var before: String?
            for _ in 0..<maxPages {
                let page = try await client.listEvents(EventQuery(before: before, order: .desc, limit: pageSize))
                if let i = page.events.firstIndex(where: { $0.id == eventID }) {
                    let exact = try await client.listEvents(EventQuery(before: before, order: .desc, limit: i + 1))
                    if exact.events.last?.id == eventID { return exact.nextCursor }
                    continue attempt // a new event shifted the first page; look again
                }
                guard page.events.count == pageSize, let next = page.nextCursor else { return nil }
                before = next
            }
            return nil
        }
        return nil
    }
}
