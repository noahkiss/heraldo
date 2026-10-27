#if canImport(ActivityKit) && os(iOS)
import ActivityKit

/// Live Activity attributes for envelope kind progress. The app and the
/// widget extension share this type. The server names it in a push-to-start
/// (`attributes-type`), so the type name and the field names are part of
/// the push contract (internal/live/payload.go).
public struct ProgressAttributes: ActivityAttributes {
    public typealias ContentState = LiveState

    /// The progress key.
    public var key: String
    /// The server's id for this activity on this device; the app reports the
    /// update token and the end under it.
    public var instance: String
    public var source: String

    public init(key: String, instance: String, source: String) {
        self.key = key
        self.instance = instance
        self.source = source
    }
}
#endif
