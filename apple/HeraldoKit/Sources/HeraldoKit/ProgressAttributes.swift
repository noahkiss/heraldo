#if canImport(ActivityKit) && os(iOS)
import ActivityKit

/// Live Activity attributes for envelope kind progress. The app and the
/// widget extension share this type. Stage 7 maps envelope content{} onto it.
public struct ProgressAttributes: ActivityAttributes {
    public struct ContentState: Codable, Hashable {
        public var status: String
        public init(status: String) { self.status = status }
    }

    public var key: String
    public var title: String

    public init(key: String, title: String) {
        self.key = key
        self.title = title
    }
}
#endif
