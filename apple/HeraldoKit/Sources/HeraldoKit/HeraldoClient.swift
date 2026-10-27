import Foundation

/// An error from the heraldo API, or from talking to it.
public enum HeraldoError: Error, Equatable, LocalizedError {
    case notConfigured(String)
    case badURL(String)
    case http(status: Int, message: String)
    case badAnswer(String)

    public var errorDescription: String? {
        switch self {
        case .notConfigured(let what): return "\(what) is not set."
        case .badURL(let url): return "Not a server URL: \(url)"
        case .http(let status, let message): return "HTTP \(status): \(message)"
        case .badAnswer(let what): return "Unexpected answer: \(what)"
        }
    }

    /// The HTTP status, when the server answered.
    public var status: Int? {
        if case .http(let status, _) = self { return status }
        return nil
    }
}

/// The query of `GET /v1/events`. Nil fields are left out.
public struct EventQuery: Hashable, Sendable {
    public enum Order: String, Sendable { case asc, desc }

    public var after: String?
    public var before: String?
    public var order: Order?
    public var limit: Int?
    public var source: String?
    public var kind: String?
    public var key: String?
    public var q: String?
    /// Keeps events whose `hold.state` is this, such as held.
    public var hold: String?

    public init(after: String? = nil, before: String? = nil, order: Order? = nil, limit: Int? = nil,
                source: String? = nil, kind: String? = nil, key: String? = nil, q: String? = nil,
                hold: String? = nil) {
        self.after = after
        self.before = before
        self.order = order
        self.limit = limit
        self.source = source
        self.kind = kind
        self.key = key
        self.q = q
        self.hold = hold
    }

    var items: [URLQueryItem] {
        let pairs: [(String, String?)] = [
            ("after", after), ("before", before), ("order", order?.rawValue),
            ("limit", limit.map(String.init)), ("source", source), ("kind", kind),
            ("key", key), ("q", q), ("hold", hold),
        ]
        return pairs.compactMap { name, value in
            guard let value, !value.isEmpty else { return nil }
            return URLQueryItem(name: name, value: value)
        }
    }
}

/// The heraldo read and write API. It holds a token pair, like the CLI: the
/// read token for history, cursors and devices, the write token for posting
/// events (Ack, Send event).
public struct HeraldoClient: Sendable {
    public var baseURL: URL
    public var readToken: String
    public var writeToken: String?
    public var session: URLSession

    public init(baseURL: URL, readToken: String, writeToken: String? = nil, session: URLSession = .shared) {
        self.baseURL = baseURL
        self.readToken = readToken
        self.writeToken = writeToken
        self.session = session
    }

    /// A client from the stored settings, or an error naming what is missing.
    public init(credentials: Credentials, session: URLSession = .shared) throws {
        guard let raw = credentials.serverURL?.trimmingCharacters(in: .whitespacesAndNewlines), !raw.isEmpty else {
            throw HeraldoError.notConfigured("The server URL")
        }
        guard let url = URL(string: raw), let scheme = url.scheme?.lowercased(),
              scheme == "http" || scheme == "https", url.host != nil else {
            throw HeraldoError.badURL(raw)
        }
        guard let read = credentials.readToken, !read.isEmpty else {
            throw HeraldoError.notConfigured("The read token")
        }
        let write = credentials.writeToken.flatMap { $0.isEmpty ? nil : $0 }
        self.init(baseURL: url, readToken: read, writeToken: write, session: session)
    }

    // MARK: Events

    public func listEvents(_ query: EventQuery = EventQuery()) async throws -> EventPage {
        try await get("v1/events", query: query.items)
    }

    public func event(id: String) async throws -> Event {
        try await get("v1/events/\(escape(id))")
    }

    public func state() async throws -> ServerState {
        try await get("v1/state")
    }

    public func health() async throws -> Health {
        try await send(request("healthz", method: "GET", token: nil))
    }

    /// Posts an event with the write token.
    @discardableResult
    public func post(_ input: EventInput) async throws -> Event {
        guard let writeToken else { throw HeraldoError.notConfigured("The write token") }
        var r = request("v1/events", method: "POST", token: writeToken)
        r.httpBody = try JSONEncoder().encode(input)
        return try await send(r)
    }

    /// Proves the write token without storing anything. It posts an empty
    /// object: the server checks the token first (401 unknown, 403 wrong
    /// scope), then refuses the body with 400 before it stores anything.
    public func checkWriteToken() async throws {
        guard let writeToken else { throw HeraldoError.notConfigured("The write token") }
        var r = request("v1/events", method: "POST", token: writeToken)
        r.httpBody = Data("{}".utf8)
        do {
            let _: Event = try await send(r)
        } catch let e as HeraldoError where e.status == 400 {
            return
        }
        throw HeraldoError.badAnswer("the server stored an empty event")
    }

    /// Acknowledges an open incident: an incident event with action ack and
    /// the incident's source and key, as `heraldo ack` posts it.
    @discardableResult
    public func ack(_ incident: Event, source: String) async throws -> Event {
        guard let key = incident.key, !key.isEmpty else {
            throw HeraldoError.badAnswer("the incident has no key")
        }
        let title = String("Acknowledged: \(incident.title ?? key)".prefix(256))
        return try await post(EventInput(source: source, kind: "incident", key: key, action: "ack",
                                         host: incident.host, title: title, level: Level.passive.rawValue))
    }

    /// The cursor of the newest event, or nil when the server has none.
    public func headCursor() async throws -> String? {
        try await listEvents(EventQuery(order: .desc, limit: 1)).nextCursor
    }

    // MARK: Check-ins

    /// Every check, in key order, or nil from a server before check-ins.
    public func checks() async throws -> [Check]? {
        do {
            let list: CheckList = try await get("v1/checkins")
            return list.checkins
        } catch let e as HeraldoError where e.status == 404 || e.status == 405 {
            return nil
        }
    }

    /// Pauses or resumes a check with the write token.
    @discardableResult
    public func setPaused(_ key: String, _ paused: Bool) async throws -> Check {
        guard let writeToken else { throw HeraldoError.notConfigured("The write token") }
        return try await send(request("v1/checkins/\(escape(key))/\(paused ? "pause" : "resume")", method: "POST", token: writeToken))
    }

    // MARK: Read cursors

    /// The read cursor called `name`, or nil when it does not exist yet.
    public func cursor(_ name: String) async throws -> ReadCursor? {
        do {
            return try await get("v1/cursors/\(escape(name))")
        } catch let e as HeraldoError where e.status == 404 {
            return nil
        }
    }

    /// Moves the read cursor forward. The answer is the stored cursor, which
    /// stays put when `cursor` is at or before it.
    @discardableResult
    public func moveCursor(_ name: String, to cursor: String) async throws -> ReadCursor {
        var r = request("v1/cursors/\(escape(name))", method: "PUT", token: readToken)
        r.httpBody = try JSONEncoder().encode(ReadCursor(cursor: cursor))
        return try await send(r)
    }

    // MARK: Devices

    public func register(_ registration: DeviceRegistration) async throws -> Device {
        var r = request("v1/devices", method: "POST", token: readToken)
        r.httpBody = try JSONEncoder().encode(registration)
        return try await send(r)
    }

    public func updateDevice(id: String, _ registration: DeviceRegistration) async throws -> Device {
        var r = request("v1/devices/\(escape(id))", method: "PUT", token: readToken)
        r.httpBody = try JSONEncoder().encode(registration)
        return try await send(r)
    }

    // MARK: Mutes

    /// Holds back pushes of a source, a key or both, with the write token.
    @discardableResult
    public func mute(_ input: MuteInput) async throws -> Mute {
        guard let writeToken else { throw HeraldoError.notConfigured("The write token") }
        var r = request("v1/mutes", method: "POST", token: writeToken)
        r.httpBody = try JSONEncoder().encode(input)
        return try await send(r)
    }

    /// The mutes in force, soonest to end first, with the write token.
    public func mutes() async throws -> [Mute] {
        guard let writeToken else { throw HeraldoError.notConfigured("The write token") }
        let list: MuteList = try await send(request("v1/mutes", method: "GET", token: writeToken))
        return list.mutes
    }

    /// Ends a mute now, with the write token.
    @discardableResult
    public func endMute(id: String) async throws -> Mute {
        guard let writeToken else { throw HeraldoError.notConfigured("The write token") }
        return try await send(request("v1/mutes/\(escape(id))", method: "DELETE", token: writeToken))
    }

    // MARK: Icons

    /// The registry icon called `name`: its image data and MIME type.
    public func icon(name: String) async throws -> (data: Data, mimeType: String?) {
        let (data, response) = try await session.data(for: request("v1/icons/\(escape(name))", method: "GET", token: readToken))
        guard let http = response as? HTTPURLResponse else { throw HeraldoError.badAnswer("not HTTP") }
        guard (200..<300).contains(http.statusCode), !data.isEmpty else {
            throw HeraldoError.http(status: http.statusCode, message: HTTPURLResponse.localizedString(forStatusCode: http.statusCode))
        }
        return (data, http.mimeType)
    }

    // MARK: Plumbing

    private func escape(_ s: String) -> String {
        s.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed.subtracting(CharacterSet(charactersIn: "/"))) ?? s
    }

    private func request(_ path: String, method: String, token: String?, query: [URLQueryItem] = []) -> URLRequest {
        var url = baseURL
        for part in path.split(separator: "/") {
            url.append(path: String(part), directoryHint: .notDirectory)
        }
        if !query.isEmpty, var c = URLComponents(url: url, resolvingAgainstBaseURL: false) {
            c.queryItems = query
            url = c.url ?? url
        }
        var r = URLRequest(url: url, timeoutInterval: 30)
        r.httpMethod = method
        r.setValue("application/json", forHTTPHeaderField: "Accept")
        if method != "GET" { r.setValue("application/json", forHTTPHeaderField: "Content-Type") }
        if let token { r.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization") }
        return r
    }

    private func get<T: Decodable>(_ path: String, query: [URLQueryItem] = []) async throws -> T {
        try await send(request(path, method: "GET", token: readToken, query: query))
    }

    private func send<T: Decodable>(_ r: URLRequest) async throws -> T {
        let (data, response) = try await session.data(for: r)
        guard let http = response as? HTTPURLResponse else { throw HeraldoError.badAnswer("not HTTP") }
        guard (200..<300).contains(http.statusCode) else {
            let message = (try? JSONDecoder().decode(ErrorBody.self, from: data).error)
                ?? HTTPURLResponse.localizedString(forStatusCode: http.statusCode)
            throw HeraldoError.http(status: http.statusCode, message: message)
        }
        do {
            return try JSON.decoder.decode(T.self, from: data)
        } catch {
            throw HeraldoError.badAnswer("\(T.self): \(error)")
        }
    }

    private struct ErrorBody: Decodable { var error: String }
}

/// JSON coding with the server's RFC 3339 dates, with or without fractions.
public enum JSON {
    public static let decoder: JSONDecoder = {
        let d = JSONDecoder()
        d.dateDecodingStrategy = .custom { decoder in
            let s = try decoder.singleValueContainer().decode(String.self)
            if let date = parseDate(s) { return date }
            throw DecodingError.dataCorrupted(.init(codingPath: decoder.codingPath, debugDescription: "bad date \(s)"))
        }
        return d
    }()

    public static let encoder: JSONEncoder = {
        let e = JSONEncoder()
        e.dateEncodingStrategy = .custom { date, encoder in
            var c = encoder.singleValueContainer()
            try c.encode(formatDate(date))
        }
        e.outputFormatting = [.sortedKeys]
        return e
    }()

    public static func parseDate(_ s: String) -> Date? {
        if let d = try? Date(s, strategy: .iso8601.year().month().day().time(includingFractionalSeconds: true).timeZone(separator: .omitted)) {
            return d
        }
        if let d = try? Date(s, strategy: .iso8601) { return d }
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f.date(from: s)
    }

    public static func formatDate(_ d: Date) -> String {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f.string(from: d)
    }
}
