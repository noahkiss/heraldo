import AppIntents
import Foundation
import HeraldoKit

/// Shortcuts: start, update, end and read a Live Activity, with Notify!'s
/// parameters. Each posts a progress event to the server with the write
/// token, and the server drives the activity on every iPhone: a Mac can start
/// one on the phone. The activity id is the progress key.

enum LiveClearField: String, AppEnum {
    case countdown, stages, metrics, button, trailing, status

    static let typeDisplayRepresentation: TypeDisplayRepresentation = "Field"
    static let caseDisplayRepresentations: [LiveClearField: DisplayRepresentation] = [
        .countdown: "Countdown", .stages: "Steps", .metrics: "Metrics",
        .button: "Button", .trailing: "Trailing text", .status: "Status",
    ]

    var clear: LiveFields.Clear { LiveFields.Clear(rawValue: rawValue)! }
}

/// The key the last Start Live Activity returned: Update and End use it
/// when no activity id is given.
enum LastLiveActivity {
    private static let name = "lastLiveActivityKey"
    static var key: String? {
        get { UserDefaults.standard.string(forKey: name) }
        set { UserDefaults.standard.set(newValue, forKey: name) }
    }

    static func resolve(_ id: String?) throws -> String {
        if let id = id?.trimmingCharacters(in: .whitespaces), !id.isEmpty { return id }
        if let key { return key }
        throw HeraldoError.notConfigured("The activity id")
    }
}

private func post(_ action: String, key: String, source: String, title: String?, content: LivePatch) async throws {
    let client = try HeraldoClient(credentials: CredentialStore().load())
    try await client.post(EventInput(source: source, kind: "progress", key: key, action: action,
                                     title: title.map { String($0.prefix(256)) }, content: content))
}

struct StartLiveActivityIntent: AppIntent {
    static let title: LocalizedStringResource = "Start Live Activity"
    static let description = IntentDescription(
        "Starts a Live Activity on your iPhone through the heraldo server. Returns the activity id.")

    @Parameter(title: "Title") var activityTitle: String
    @Parameter(title: "Body") var body: String?
    @Parameter(title: "Symbol", description: "An SF Symbol name, such as printer.fill.") var symbol: String?
    @Parameter(title: "Progress", description: "Percent done, 0 to 100.", inclusiveRange: (0, 100)) var progress: Double?
    @Parameter(title: "Ends In", description: "Seconds until the work ends; shows a countdown.") var endsIn: Int?
    @Parameter(title: "Steps", description: "How many steps there are.") var steps: Int?
    @Parameter(title: "Step", description: "The current step.") var step: Int?
    @Parameter(title: "Tint", description: "A color name, such as orange, or #RRGGBB.") var tint: String?
    @Parameter(title: "Trailing Text") var trailing: String?
    @Parameter(title: "Status") var status: String?
    @Parameter(title: "Metrics", description: "Up to six, such as Nozzle: 215 °C, Bed: 60 °C.") var metricsText: String?
    @Parameter(title: "Button Title") var buttonTitle: String?
    @Parameter(title: "Button URL") var buttonUrl: String?
    @Parameter(title: "Button Opens Link", description: "On: the button opens the URL. Off: the phone calls it.", default: true)
    var buttonOpensLink: Bool
    @Parameter(title: "Activity Id", description: "Leave empty for a new id.") var activityId: String?
    @Parameter(title: "Source", default: "shortcuts") var source: String

    static var parameterSummary: some ParameterSummary {
        Summary("Start Live Activity \(\.$activityTitle)") {
            \.$body
            \.$symbol
            \.$progress
            \.$endsIn
            \.$steps
            \.$step
            \.$tint
            \.$trailing
            \.$status
            \.$metricsText
            \.$buttonTitle
            \.$buttonUrl
            \.$buttonOpensLink
            \.$activityId
            \.$source
        }
    }

    func perform() async throws -> some IntentResult & ReturnsValue<String> {
        var f = LiveFields()
        f.title = activityTitle
        f.body = body
        f.symbol = symbol
        f.progress = progress
        f.endsIn = endsIn
        f.steps = steps
        f.step = step
        f.tint = tint
        f.trailing = trailing
        f.status = status
        f.metricsText = metricsText
        f.buttonTitle = buttonTitle
        f.buttonURL = buttonUrl
        f.buttonOpensLink = buttonOpensLink
        let key = activityId.flatMap { $0.isEmpty ? nil : $0 } ?? "shortcuts/" + UUID().uuidString.prefix(8).lowercased()
        try await post("start", key: key, source: source, title: activityTitle, content: f.patch())
        LastLiveActivity.key = key
        return .result(value: key)
    }
}

struct UpdateLiveActivityIntent: AppIntent {
    static let title: LocalizedStringResource = "Update Live Activity"
    static let description = IntentDescription(
        "Changes a Live Activity. Fields left empty keep their value. Without an id, updates the last one started.")

    @Parameter(title: "Activity Id") var activityId: String?
    @Parameter(title: "Title") var activityTitle: String?
    @Parameter(title: "Body") var body: String?
    @Parameter(title: "Symbol") var symbol: String?
    @Parameter(title: "Progress", description: "Percent done, 0 to 100.", inclusiveRange: (0, 100)) var progress: Double?
    @Parameter(title: "Ends In", description: "Seconds until the work ends.") var endsIn: Int?
    @Parameter(title: "Steps") var steps: Int?
    @Parameter(title: "Step") var step: Int?
    @Parameter(title: "Tint") var tint: String?
    @Parameter(title: "Trailing Text") var trailing: String?
    @Parameter(title: "Status") var status: String?
    @Parameter(title: "Metrics") var metricsText: String?
    @Parameter(title: "Button Title") var buttonTitle: String?
    @Parameter(title: "Button URL") var buttonUrl: String?
    @Parameter(title: "Button Opens Link", default: true) var buttonOpensLink: Bool
    @Parameter(title: "Clear Fields") var clearFields: [LiveClearField]?
    @Parameter(title: "Source", default: "shortcuts") var source: String

    static var parameterSummary: some ParameterSummary {
        Summary("Update Live Activity \(\.$activityId)") {
            \.$activityTitle
            \.$body
            \.$symbol
            \.$progress
            \.$endsIn
            \.$steps
            \.$step
            \.$tint
            \.$trailing
            \.$status
            \.$metricsText
            \.$buttonTitle
            \.$buttonUrl
            \.$buttonOpensLink
            \.$clearFields
            \.$source
        }
    }

    func perform() async throws -> some IntentResult & ReturnsValue<String> {
        let key = try LastLiveActivity.resolve(activityId)
        var f = LiveFields()
        f.title = activityTitle
        f.body = body
        f.symbol = symbol
        f.progress = progress
        f.endsIn = endsIn
        f.steps = steps
        f.step = step
        f.tint = tint
        f.trailing = trailing
        f.status = status
        f.metricsText = metricsText
        f.buttonTitle = buttonTitle
        f.buttonURL = buttonUrl
        f.buttonOpensLink = buttonOpensLink
        try await post("update", key: key, source: source, title: nil,
                       content: f.patch(clearing: Set((clearFields ?? []).map(\.clear))))
        return .result(value: key)
    }
}

struct EndLiveActivityIntent: AppIntent {
    static let title: LocalizedStringResource = "End Live Activity"
    static let description = IntentDescription(
        "Ends a Live Activity, with optional final content. Without an id, ends the last one started.")

    @Parameter(title: "Activity Id") var activityId: String?
    @Parameter(title: "Keep For", description: "Seconds the ended activity stays on screen.") var keepFor: Int?
    @Parameter(title: "Title") var activityTitle: String?
    @Parameter(title: "Body") var body: String?
    @Parameter(title: "Progress", inclusiveRange: (0, 100)) var progress: Double?
    @Parameter(title: "Tint") var tint: String?
    @Parameter(title: "Status") var status: String?
    @Parameter(title: "Source", default: "shortcuts") var source: String

    static var parameterSummary: some ParameterSummary {
        Summary("End Live Activity \(\.$activityId)") {
            \.$keepFor
            \.$activityTitle
            \.$body
            \.$progress
            \.$tint
            \.$status
            \.$source
        }
    }

    func perform() async throws -> some IntentResult & ReturnsValue<String> {
        let key = try LastLiveActivity.resolve(activityId)
        var f = LiveFields()
        f.title = activityTitle
        f.body = body
        f.progress = progress
        f.tint = tint
        f.status = status
        var content = f.patch(clearing: [.countdown])
        if let k = keepFor, k >= 0 { content.keepFor = .set(k) }
        try await post("end", key: key, source: source, title: nil, content: content)
        if LastLiveActivity.key == key { LastLiveActivity.key = nil }
        return .result(value: key)
    }
}

struct GetLiveActivityIntent: AppIntent {
    static let title: LocalizedStringResource = "Get Live Activity"
    static let description = IntentDescription(
        "Returns a Live Activity's state as JSON: active or ended, why it ended, and each device's copy.")

    @Parameter(title: "Activity Id") var activityId: String?

    static var parameterSummary: some ParameterSummary {
        Summary("Get Live Activity \(\.$activityId)")
    }

    func perform() async throws -> some IntentResult & ReturnsValue<String> {
        let key = try LastLiveActivity.resolve(activityId)
        let client = try HeraldoClient(credentials: CredentialStore().load())
        let info = try await client.activity(key: key)
        return .result(value: String(decoding: try JSON.encoder.encode(info), as: UTF8.self))
    }
}
