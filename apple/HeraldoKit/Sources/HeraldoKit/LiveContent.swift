import Foundation

/// What a Live Activity shows: the content state the server pushes
/// (docs/api.md, "Live Activities"). The field names are the envelope's
/// LiveContent names. `ends_at` is Unix seconds here, not RFC 3339 text,
/// because ActivityKit decodes a pushed state with a plain decoder.
public struct LiveState: Codable, Hashable, Sendable {
    public var title: String?
    public var symbol: String?
    public var tint: String?
    public var body: String?
    /// Fraction done, 0 to 1.
    public var progress: Double?
    public var steps: LiveSteps?
    public var endsAt: Date?
    public var trailing: String?
    public var status: String?
    public var metrics: [LiveMetric]?
    public var button: LiveButton?

    enum CodingKeys: String, CodingKey {
        case title, symbol, tint, body, progress, steps, trailing, status, metrics, button
        case endsAt = "ends_at"
    }

    public init(title: String? = nil, symbol: String? = nil, tint: String? = nil, body: String? = nil,
                progress: Double? = nil, steps: LiveSteps? = nil, endsAt: Date? = nil, trailing: String? = nil,
                status: String? = nil, metrics: [LiveMetric]? = nil, button: LiveButton? = nil) {
        self.title = title
        self.symbol = symbol
        self.tint = tint
        self.body = body
        self.progress = progress
        self.steps = steps
        self.endsAt = endsAt
        self.trailing = trailing
        self.status = status
        self.metrics = metrics
        self.button = button
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        title = try c.decodeIfPresent(String.self, forKey: .title)
        symbol = try c.decodeIfPresent(String.self, forKey: .symbol)
        tint = try c.decodeIfPresent(String.self, forKey: .tint)
        body = try c.decodeIfPresent(String.self, forKey: .body)
        progress = try c.decodeIfPresent(Double.self, forKey: .progress)
        steps = try c.decodeIfPresent(LiveSteps.self, forKey: .steps)
        endsAt = try c.decodeIfPresent(Double.self, forKey: .endsAt).map { Date(timeIntervalSince1970: $0) }
        trailing = try c.decodeIfPresent(String.self, forKey: .trailing)
        status = try c.decodeIfPresent(String.self, forKey: .status)
        // A metric the app cannot read is left out, not the whole state.
        metrics = try c.decodeIfPresent([Lenient<LiveMetric>].self, forKey: .metrics)?.compactMap(\.value)
        button = try? c.decodeIfPresent(LiveButton.self, forKey: .button)
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encodeIfPresent(title, forKey: .title)
        try c.encodeIfPresent(symbol, forKey: .symbol)
        try c.encodeIfPresent(tint, forKey: .tint)
        try c.encodeIfPresent(body, forKey: .body)
        try c.encodeIfPresent(progress, forKey: .progress)
        try c.encodeIfPresent(steps, forKey: .steps)
        try c.encodeIfPresent(endsAt.map { $0.timeIntervalSince1970.rounded() }, forKey: .endsAt)
        try c.encodeIfPresent(trailing, forKey: .trailing)
        try c.encodeIfPresent(status, forKey: .status)
        try c.encodeIfPresent(metrics, forKey: .metrics)
        try c.encodeIfPresent(button, forKey: .button)
    }

    /// The fraction to draw: progress, else steps, clamped to 0...1. Nil
    /// when the state has neither.
    public var fraction: Double? {
        if let p = progress { return min(max(p, 0), 1) }
        if let s = steps, s.total > 0 { return min(max(Double(s.current) / Double(s.total), 0), 1) }
        return nil
    }

    /// Short progress text: "3 of 120" for steps, else "42%".
    public var progressText: String? {
        if let s = steps { return "\(s.current) of \(s.total)" }
        if let f = fraction { return "\(Int((f * 100).rounded()))%" }
        return nil
    }

    /// The metrics to show: at most six.
    public var shownMetrics: [LiveMetric] { Array((metrics ?? []).prefix(6)) }

    /// The countdown, while it is still running at `now`.
    public func countdown(at now: Date) -> ClosedRange<Date>? {
        guard let end = endsAt, end > now else { return nil }
        return now...end
    }
}

public struct LiveSteps: Codable, Hashable, Sendable {
    public var total: Int
    public var current: Int

    public init(total: Int, current: Int) {
        self.total = total
        self.current = current
    }
}

public struct LiveMetric: Codable, Hashable, Sendable {
    public var label: String
    public var value: String
    public var unit: String?
    public var color: String?

    public init(label: String, value: String, unit: String? = nil, color: String? = nil) {
        self.label = label
        self.value = value
        self.unit = unit
        self.color = color
    }

    /// "215 °C": the value with its unit.
    public var text: String {
        guard let unit, !unit.isEmpty else { return value }
        return unit.first?.isLetter == true || unit.first == "°" ? "\(value) \(unit)" : value + unit
    }
}

/// The activity's one button: an envelope Action.
public struct LiveButton: Codable, Hashable, Sendable {
    public var id: String
    public var title: String
    /// open, http or server.
    public var type: String
    public var url: String?
    public var method: String?

    public init(id: String, title: String, type: String, url: String? = nil, method: String? = nil) {
        self.id = id
        self.title = title
        self.type = type
        self.url = url
        self.method = method
    }
}

/// An activity's tint: a system color name or an RGBA value.
public enum LiveTint: Equatable, Sendable {
    case named(String)
    case rgba(red: Double, green: Double, blue: Double, alpha: Double)

    /// The system color names the schema allows.
    public static let names: Set<String> = [
        "red", "orange", "yellow", "green", "mint", "teal", "cyan", "blue", "indigo", "purple", "pink",
        "brown", "gray", "primary", "secondary",
    ]

    /// Parses `#RRGGBB`, `#RRGGBBAA` or a system color name. Nil for anything else.
    public init?(_ s: String?) {
        guard let s = s?.trimmingCharacters(in: .whitespaces), !s.isEmpty else { return nil }
        if Self.names.contains(s.lowercased()) {
            self = .named(s.lowercased())
            return
        }
        guard s.hasPrefix("#"), s.count == 7 || s.count == 9, let v = UInt64(s.dropFirst(), radix: 16) else {
            return nil
        }
        let hasAlpha = s.count == 9
        let rgb = hasAlpha ? v >> 8 : v
        self = .rgba(red: Double((rgb >> 16) & 0xFF) / 255, green: Double((rgb >> 8) & 0xFF) / 255,
                     blue: Double(rgb & 0xFF) / 255, alpha: hasAlpha ? Double(v & 0xFF) / 255 : 1)
    }
}

/// Decodes a value, or nil when it does not decode.
struct Lenient<T: Decodable>: Decodable {
    var value: T?
    init(from decoder: Decoder) throws { value = try? T(from: decoder) }
}

// MARK: Sending progress

/// One field of a progress update: left alone, cleared, or set.
public enum Patch<Value: Encodable & Hashable & Sendable>: Hashable, Sendable {
    case keep
    case clear
    case set(Value)

    /// .set for a value, .keep for nil.
    public static func from(_ v: Value?) -> Patch { v.map(Patch.set) ?? .keep }
}

/// The `content` of a progress event the app posts. On an update a kept
/// field is left out and a cleared one is sent as null, as the server merges
/// them (docs/api.md, "LiveContent").
public struct LivePatch: Encodable, Hashable, Sendable {
    public var title: Patch<String> = .keep
    public var symbol: Patch<String> = .keep
    public var tint: Patch<String> = .keep
    public var body: Patch<String> = .keep
    public var progress: Patch<Double> = .keep
    public var steps: Patch<LiveSteps> = .keep
    public var endsAt: Patch<Date> = .keep
    public var trailing: Patch<String> = .keep
    public var status: Patch<String> = .keep
    public var metrics: Patch<[LiveMetric]> = .keep
    public var button: Patch<LiveButton> = .keep
    public var keepFor: Patch<Int> = .keep

    public init() {}

    enum CodingKeys: String, CodingKey {
        case title, symbol, tint, body, progress, steps, trailing, status, metrics, button
        case endsAt = "ends_at"
        case keepFor = "keep_for"
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try put(title, .title, &c)
        try put(symbol, .symbol, &c)
        try put(tint, .tint, &c)
        try put(body, .body, &c)
        try put(progress, .progress, &c)
        try put(steps, .steps, &c)
        // The envelope's ends_at is RFC 3339.
        switch endsAt {
        case .keep: break
        case .clear: try c.encodeNil(forKey: .endsAt)
        case .set(let d): try c.encode(JSON.formatDate(d), forKey: .endsAt)
        }
        try put(trailing, .trailing, &c)
        try put(status, .status, &c)
        try put(metrics, .metrics, &c)
        try put(button, .button, &c)
        try put(keepFor, .keepFor, &c)
    }

    private func put<V>(_ p: Patch<V>, _ key: CodingKeys, _ c: inout KeyedEncodingContainer<CodingKeys>) throws {
        switch p {
        case .keep: break
        case .clear: try c.encodeNil(forKey: key)
        case .set(let v): try c.encode(v, forKey: key)
        }
    }

    /// Whether every field is left alone.
    public var isEmpty: Bool { self == LivePatch() }
}

public enum LiveText {
    /// Metrics from one line of text, as a Shortcut passes them:
    /// "Nozzle: 215 °C, Bed: 60 °C". Pairs split on commas, semicolons or
    /// new lines; label and value on the first colon or equals sign. At most
    /// six; a pair without a label or a value is skipped.
    public static func metrics(_ text: String) -> [LiveMetric] {
        var out: [LiveMetric] = []
        for part in text.split(whereSeparator: { ",;\n".contains($0) }) {
            guard let sep = part.firstIndex(where: { $0 == ":" || $0 == "=" }) else { continue }
            let label = part[..<sep].trimmingCharacters(in: .whitespaces)
            let value = part[part.index(after: sep)...].trimmingCharacters(in: .whitespaces)
            guard !label.isEmpty, !value.isEmpty else { continue }
            out.append(LiveMetric(label: String(label.prefix(40)), value: String(value.prefix(40))))
            if out.count == 6 { break }
        }
        return out
    }
}
