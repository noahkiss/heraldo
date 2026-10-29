import Foundation

/// A server setting's value, in the key's JSON type.
public enum SettingValue: Codable, Hashable, Sendable {
    case bool(Bool)
    case int(Int)
    case string(String)
    case list([String])

    public init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if let b = try? c.decode(Bool.self) { self = .bool(b); return }
        if let n = try? c.decode(Int.self) { self = .int(n); return }
        if let s = try? c.decode(String.self) { self = .string(s); return }
        if let l = try? c.decode([String].self) { self = .list(l); return }
        throw DecodingError.dataCorruptedError(in: c, debugDescription: "not a setting value")
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .bool(let b): try c.encode(b)
        case .int(let n): try c.encode(n)
        case .string(let s): try c.encode(s)
        case .list(let l): try c.encode(l)
        }
    }

    public var bool: Bool? { if case .bool(let b) = self { return b }; return nil }
    public var string: String? { if case .string(let s) = self { return s }; return nil }
    public var list: [String]? { if case .list(let l) = self { return l }; return nil }
}

/// One server setting from `GET /v1/settings`.
public struct ServerSetting: Codable, Hashable, Sendable, Identifiable {
    public var key: String
    public var type: String
    public var value: SettingValue?
    public var source: String
    /// The flag or env var that sets the key; while set, writes answer 409.
    public var lockedBy: String?
    public var `enum`: [String]?
    public var description: String
    /// For a secret, whose value is never read back: whether it holds one.
    public var set: Bool?

    public var id: String { key }
    public var isLocked: Bool { !(lockedBy ?? "").isEmpty }

    enum CodingKeys: String, CodingKey {
        case key, type, value, source, description
        case lockedBy = "locked_by"
        case `enum`
        case set
    }

    public init(key: String, type: String, value: SettingValue?, source: String = "default",
                lockedBy: String? = nil, enum values: [String]? = nil, description: String = "", set: Bool? = nil) {
        self.key = key
        self.type = type
        self.value = value
        self.source = source
        self.lockedBy = lockedBy
        self.enum = values
        self.description = description
        self.set = set
    }
}

struct ServerSettingList: Codable, Sendable {
    var settings: [ServerSetting]
}

struct SettingInput: Codable, Sendable {
    var value: SettingValue
}

/// A device's Focus filter, as the app's Focus filter intent reports it with
/// `PUT /v1/devices/{id}`. `active` false clears it on the server. While one
/// is set, the server sends this device's pushes above it at active.
public struct FocusFilterState: Codable, Hashable, Sendable {
    /// `break_through` values.
    public static let timeSensitive = "time-sensitive"
    public static let critical = "critical"

    public var active: Bool
    public var name: String?
    /// Projects whose events may break through; nil or empty is every one.
    public var projects: [String]?
    /// time-sensitive (the default) or critical.
    public var breakThrough: String?

    enum CodingKeys: String, CodingKey {
        case active, name, projects
        case breakThrough = "break_through"
    }

    public init(active: Bool, name: String? = nil, projects: [String]? = nil, breakThrough: String? = nil) {
        self.active = active
        self.name = name
        self.projects = projects
        self.breakThrough = breakThrough
    }

    /// The filter for a Focus with these parameters. The defaults (every
    /// project, time-sensitive and up) change nothing, so they clear the
    /// filter: the system calls the intent with the defaults when the Focus
    /// ends.
    public static func from(projects: [String], breakThrough: String) -> FocusFilterState {
        let changesNothing = projects.isEmpty && breakThrough != critical
        if changesNothing { return FocusFilterState(active: false) }
        return FocusFilterState(active: true, projects: projects.isEmpty ? nil : projects, breakThrough: breakThrough)
    }
}
