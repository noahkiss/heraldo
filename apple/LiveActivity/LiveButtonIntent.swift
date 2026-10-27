#if os(iOS)
import AppIntents
import Foundation

/// The Live Activity button of type http: the device calls the URL, so it
/// can reach hosts on the local network. Compiled into the app and the
/// widget extension; a LiveActivityIntent runs in the app's process.
struct LiveButtonIntent: LiveActivityIntent {
    static let title: LocalizedStringResource = "Live Activity Button"
    static let isDiscoverable = false

    @Parameter(title: "URL")
    var url: String

    @Parameter(title: "Method", default: "POST")
    var method: String

    init() {}

    init(url: String, method: String?) {
        self.url = url
        self.method = method ?? "POST"
    }

    func perform() async throws -> some IntentResult {
        guard let u = URL(string: url), let scheme = u.scheme?.lowercased(), scheme == "http" || scheme == "https" else {
            throw LiveButtonError.badURL
        }
        var r = URLRequest(url: u, timeoutInterval: 15)
        r.httpMethod = method.uppercased() == "GET" ? "GET" : "POST"
        r.setValue("Heraldo-LiveActivityButton/1", forHTTPHeaderField: "User-Agent")
        let (_, response) = try await URLSession.shared.data(for: r)
        if let http = response as? HTTPURLResponse, !(200..<300).contains(http.statusCode) {
            throw LiveButtonError.status(http.statusCode)
        }
        return .result()
    }
}

enum LiveButtonError: Error, CustomLocalizedStringResourceConvertible {
    case badURL
    case status(Int)

    var localizedStringResource: LocalizedStringResource {
        switch self {
        case .badURL: return "The button has no http or https URL."
        case .status(let code): return "The button's request failed with HTTP \(code)."
        }
    }
}
#endif
