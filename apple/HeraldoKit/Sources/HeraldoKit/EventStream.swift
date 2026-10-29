import Foundation

/// One dispatched server-sent event.
public struct SSEEvent: Equatable, Sendable {
    /// The `event:` field; `message` when the server named none.
    public var event: String
    /// The `data:` lines, joined with newlines.
    public var data: String
    public var id: String?

    public init(event: String = "message", data: String, id: String? = nil) {
        self.event = event
        self.data = data
        self.id = id
    }
}

/// What one line of a stream produced.
public enum SSEOutput: Equatable, Sendable {
    case event(SSEEvent)
    /// A line that starts with a colon, such as `: keep-alive`.
    case comment(String)
}

/// The line parser of the server-sent events format (WHATWG HTML,
/// "Server-sent events"). Lines end with LF, CR or CRLF; a blank line
/// dispatches the event. Feed it bytes as they arrive.
public struct SSEParser: Sendable {
    private var line: [UInt8] = []
    private var afterCR = false
    private var event = ""
    private var data: [String] = []
    private var hasData = false
    private var id: String?
    /// The last `retry:` value, in milliseconds.
    public private(set) var retry: Int?

    public init() {}

    /// Takes one byte. Returns what the line it ends produced, if any.
    public mutating func feed(_ byte: UInt8) -> SSEOutput? {
        switch byte {
        case 0x0A: // LF
            if afterCR { afterCR = false; return nil }
            return endLine()
        case 0x0D: // CR
            afterCR = true
            return endLine()
        default:
            afterCR = false
            line.append(byte)
            return nil
        }
    }

    /// Takes a chunk of bytes; returns everything it produced, in order.
    public mutating func feed<S: Sequence>(_ bytes: S) -> [SSEOutput] where S.Element == UInt8 {
        var out: [SSEOutput] = []
        for b in bytes { if let o = feed(b) { out.append(o) } }
        return out
    }

    private mutating func endLine() -> SSEOutput? {
        let text = String(decoding: line, as: UTF8.self)
        line.removeAll(keepingCapacity: true)
        return process(text)
    }

    /// Processes one complete line, without its line ending.
    public mutating func process(_ text: String) -> SSEOutput? {
        if text.isEmpty { return dispatch() }
        if text.hasPrefix(":") {
            var c = text.dropFirst()
            if c.hasPrefix(" ") { c = c.dropFirst() }
            return .comment(String(c))
        }
        let field: Substring
        var value: Substring
        if let colon = text.firstIndex(of: ":") {
            field = text[..<colon]
            value = text[text.index(after: colon)...]
            if value.hasPrefix(" ") { value = value.dropFirst() }
        } else {
            field = Substring(text)
            value = ""
        }
        switch field {
        case "event": event = String(value)
        case "data":
            data.append(String(value))
            hasData = true
        case "id":
            if !value.contains("\0") { id = String(value) }
        case "retry":
            if let n = Int(value), n >= 0 { retry = n }
        default: break
        }
        return nil
    }

    private mutating func dispatch() -> SSEOutput? {
        defer {
            event = ""
            data = []
            hasData = false
        }
        guard hasData else { return nil }
        return .event(SSEEvent(event: event.isEmpty ? "message" : event, data: data.joined(separator: "\n"), id: id))
    }
}

/// A message of `GET /v1/stream`, as the app reads it.
public enum StreamMessage: Equatable, Sendable {
    /// The vector, sent once right after connecting.
    case revisions(Revisions)
    /// Something changed. A server before revisions sends only the cursor.
    case poke(Poke)
    /// A keep-alive comment.
    case keepAlive

    /// The message of one event, or nil for an event the app ignores.
    public init?(_ e: SSEEvent) {
        let data = Data(e.data.utf8)
        switch e.event {
        case "revisions":
            guard let v = try? JSON.decoder.decode(Revisions.self, from: data) else { return nil }
            self = .revisions(v)
        case "poke":
            // A poke whose data does not parse still says "something changed".
            self = .poke((try? JSON.decoder.decode(Poke.self, from: data)) ?? Poke())
        default:
            return nil
        }
    }
}

/// The wait before a reconnect: from `base`, doubling per failed attempt,
/// up to `cap`, with full jitter in the upper half so clients that lost the
/// server together do not return together.
public struct Backoff: Sendable {
    public var base: TimeInterval
    public var cap: TimeInterval

    public init(base: TimeInterval = 3, cap: TimeInterval = 300) {
        self.base = base
        self.cap = cap
    }

    /// The wait before attempt `attempt` (0 = the first reconnect). `unit`
    /// is a random number in 0..<1.
    public func delay(attempt: Int, unit: Double) -> TimeInterval {
        let exp = base * pow(2, Double(min(max(attempt, 0), 20)))
        let ceiling = min(cap, exp)
        return ceiling / 2 + ceiling / 2 * min(max(unit, 0), 1)
    }

    public func delay(attempt: Int) -> TimeInterval {
        delay(attempt: attempt, unit: Double.random(in: 0..<1))
    }
}

extension HeraldoClient {
    /// The heartbeats the server takes, in seconds.
    public static let heartbeatRange = 15...120

    /// The request of `GET /v1/stream`. `heartbeat` asks for a keep-alive
    /// every so many seconds; a server before the parameter ignores it. The
    /// request times out after two missed heartbeats, so a dead connection
    /// ends.
    public func streamRequest(heartbeat: Int) -> URLRequest {
        let beat = min(max(heartbeat, Self.heartbeatRange.lowerBound), Self.heartbeatRange.upperBound)
        var url = baseURL
        url.append(path: "v1", directoryHint: .notDirectory)
        url.append(path: "stream", directoryHint: .notDirectory)
        if var c = URLComponents(url: url, resolvingAgainstBaseURL: false) {
            c.queryItems = [URLQueryItem(name: "heartbeat", value: String(beat))]
            url = c.url ?? url
        }
        var r = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData,
                           timeoutInterval: TimeInterval(beat * 2 + 10))
        r.httpMethod = "GET"
        r.setValue("text/event-stream", forHTTPHeaderField: "Accept")
        r.setValue("Bearer \(readToken)", forHTTPHeaderField: "Authorization")
        return r
    }

    /// The revision vector, or nil from a server before revisions (404).
    public func revisions() async throws -> Revisions? {
        do {
            let v: Revisions = try await send(request("v1/revisions", method: "GET", token: readToken))
            return v
        } catch let e as HeraldoError where e.status == 404 || e.status == 405 {
            return nil
        }
    }
}

/// One `GET /v1/stream` connection that reconnects by itself: a dropped
/// stream comes back after `Backoff` (3 s, doubling, up to 5 min). It does
/// not watch the network; whoever owns it stops it when the path goes down
/// and starts it again when the path returns.
public final class EventStream: @unchecked Sendable {
    public enum Status: Equatable, Sendable {
        case connecting
        case open
        /// The stream ended; the next attempt is after `retryIn` seconds.
        case failed(retryIn: TimeInterval)
    }

    private let client: HeraldoClient
    private let heartbeat: Int
    private let backoff: Backoff
    private let session: URLSession
    private let onMessage: @Sendable (StreamMessage) -> Void
    private let onStatus: @Sendable (Status) -> Void
    private let lock = NSLock()
    private var task: Task<Void, Never>?

    public init(client: HeraldoClient, heartbeat: Int, backoff: Backoff = Backoff(),
                onMessage: @escaping @Sendable (StreamMessage) -> Void,
                onStatus: @escaping @Sendable (Status) -> Void = { _ in },
                configure: (URLSessionConfiguration) -> Void = { _ in }) {
        self.client = client
        self.heartbeat = heartbeat
        self.backoff = backoff
        self.onMessage = onMessage
        self.onStatus = onStatus
        let c = URLSessionConfiguration.ephemeral
        c.waitsForConnectivity = false
        c.timeoutIntervalForRequest = TimeInterval(heartbeat * 2 + 10)
        c.timeoutIntervalForResource = .infinity
        c.requestCachePolicy = .reloadIgnoringLocalCacheData
        configure(c)
        session = URLSession(configuration: c)
    }

    deinit { stop() }

    public func start() {
        lock.lock()
        defer { lock.unlock() }
        guard task == nil else { return }
        task = Task.detached(priority: .utility) { [weak self] in await self?.run() }
    }

    public func stop() {
        lock.lock()
        let t = task
        task = nil
        lock.unlock()
        t?.cancel()
    }

    private func run() async {
        var attempt = 0
        while !Task.isCancelled {
            onStatus(.connecting)
            var opened = false
            do {
                let (bytes, response) = try await session.bytes(for: client.streamRequest(heartbeat: heartbeat))
                guard let http = response as? HTTPURLResponse else { throw HeraldoError.badAnswer("not HTTP") }
                guard http.statusCode == 200 else {
                    // A server without the stream, or a refused token: wait the longest.
                    if [401, 403, 404, 405].contains(http.statusCode) { attempt = 20 }
                    throw HeraldoError.http(status: http.statusCode, message: "stream")
                }
                var parser = SSEParser()
                for try await byte in bytes {
                    guard let out = parser.feed(byte) else { continue }
                    let message: StreamMessage?
                    switch out {
                    case .comment: message = .keepAlive
                    case .event(let e): message = StreamMessage(e)
                    }
                    guard let message else { continue }
                    if !opened {
                        opened = true
                        attempt = 0
                        onStatus(.open)
                    }
                    onMessage(message)
                }
            } catch {
                // Cancelled, timed out after missed heartbeats, or dropped.
            }
            if Task.isCancelled { return }
            let wait = backoff.delay(attempt: attempt)
            attempt += 1
            onStatus(.failed(retryIn: wait))
            try? await Task.sleep(for: .seconds(wait), tolerance: .seconds(wait / 10))
        }
    }
}
