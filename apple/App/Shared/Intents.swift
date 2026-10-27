import AppIntents
import Foundation
import HeraldoKit

/// Shortcuts: send an event, get the recent history as JSON, and get the
/// open incidents as text or JSON.

enum IntentLevel: String, AppEnum {
    case passive, active
    case timeSensitive = "time-sensitive"

    static let typeDisplayRepresentation: TypeDisplayRepresentation = "Level"
    static let caseDisplayRepresentations: [IntentLevel: DisplayRepresentation] = [
        .passive: "Passive",
        .active: "Active",
        .timeSensitive: "Time Sensitive",
    ]
}

struct SendEventIntent: AppIntent {
    static let title: LocalizedStringResource = "Send Event"
    static let description = IntentDescription("Posts an event to the heraldo server with the write token.")

    @Parameter(title: "Title")
    var eventTitle: String

    @Parameter(title: "Body", inputOptions: String.IntentInputOptions(multiline: true))
    var body: String?

    @Parameter(title: "Source", default: "shortcuts")
    var source: String

    @Parameter(title: "Level", default: .active)
    var level: IntentLevel

    static var parameterSummary: some ParameterSummary {
        Summary("Send \(\.$eventTitle) from \(\.$source)") {
            \.$body
            \.$level
        }
    }

    func perform() async throws -> some IntentResult & ReturnsValue<String> {
        let client = try HeraldoClient(credentials: CredentialStore().load())
        let e = try await client.post(EventInput(source: source, title: eventTitle, body: body, level: level.rawValue))
        return .result(value: e.id)
    }
}

struct GetRecentHistoryIntent: AppIntent {
    static let title: LocalizedStringResource = "Get Recent History"
    static let description = IntentDescription("Returns the newest events from the heraldo server as JSON.")

    @Parameter(title: "Limit", default: 20, inclusiveRange: (1, 500))
    var limit: Int

    static var parameterSummary: some ParameterSummary {
        Summary("Get the newest \(\.$limit) events")
    }

    func perform() async throws -> some IntentResult & ReturnsValue<String> {
        let client = try HeraldoClient(credentials: CredentialStore().load())
        let page = try await client.listEvents(EventQuery(order: .desc, limit: limit))
        let data = try JSON.encoder.encode(page.events)
        return .result(value: String(decoding: data, as: UTF8.self))
    }
}

enum IntentFormat: String, AppEnum {
    case text, json

    static let typeDisplayRepresentation: TypeDisplayRepresentation = "Format"
    static let caseDisplayRepresentations: [IntentFormat: DisplayRepresentation] = [
        .text: "Text",
        .json: "JSON",
    ]
}

struct GetOpenIncidentsIntent: AppIntent {
    static let title: LocalizedStringResource = "Get Open Incidents"
    static let description = IntentDescription(
        "Returns the open and acknowledged incidents from the heraldo server, as one line each or as JSON.")

    @Parameter(title: "Format", default: .text)
    var format: IntentFormat

    static var parameterSummary: some ParameterSummary {
        Summary("Get open incidents as \(\.$format)")
    }

    func perform() async throws -> some IntentResult & ReturnsValue<String> {
        let client = try HeraldoClient(credentials: CredentialStore().load())
        let incidents = try await client.state().sortedIncidents
        switch format {
        case .text:
            return .result(value: ServerState.incidentsText(incidents))
        case .json:
            return .result(value: String(decoding: try JSON.encoder.encode(incidents), as: UTF8.self))
        }
    }
}

struct HeraldoShortcuts: AppShortcutsProvider {
    static var appShortcuts: [AppShortcut] {
        AppShortcut(intent: SendEventIntent(),
                    phrases: ["Send an event with \(.applicationName)", "Send a \(.applicationName) event"],
                    shortTitle: "Send Event", systemImageName: "paperplane")
        AppShortcut(intent: GetRecentHistoryIntent(),
                    phrases: ["Get recent \(.applicationName) history", "Get history from \(.applicationName)"],
                    shortTitle: "Recent History", systemImageName: "clock.arrow.circlepath")
        AppShortcut(intent: GetOpenIncidentsIntent(),
                    phrases: ["Get open incidents from \(.applicationName)", "Get \(.applicationName) incidents"],
                    shortTitle: "Open Incidents", systemImageName: "exclamationmark.triangle")
        AppShortcut(intent: StartLiveActivityIntent(),
                    phrases: ["Start a Live Activity with \(.applicationName)", "Start a \(.applicationName) Live Activity"],
                    shortTitle: "Start Live Activity", systemImageName: "livephoto.play")
        AppShortcut(intent: UpdateLiveActivityIntent(),
                    phrases: ["Update a Live Activity with \(.applicationName)", "Update the \(.applicationName) Live Activity"],
                    shortTitle: "Update Live Activity", systemImageName: "arrow.triangle.2.circlepath")
        AppShortcut(intent: EndLiveActivityIntent(),
                    phrases: ["End a Live Activity with \(.applicationName)", "End the \(.applicationName) Live Activity"],
                    shortTitle: "End Live Activity", systemImageName: "stop.circle")
        AppShortcut(intent: GetLiveActivityIntent(),
                    phrases: ["Get a Live Activity from \(.applicationName)"],
                    shortTitle: "Get Live Activity", systemImageName: "info.circle")
    }
}
