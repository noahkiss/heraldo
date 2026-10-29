import AppIntents
import Foundation
import HeraldoKit

/// The Focus filter: Settings > Focus > a Focus > Focus Filters > Heraldo.
/// While the Focus is on, the server sends this device's pushes above the
/// filter at active, so the Focus silences them. Critical always breaks
/// through. With the defaults (every project, time-sensitive and up) the
/// filter changes nothing, and the app tells the server to clear it.

/// A project from `GET /v1/projects`, to pick in the Focus filter.
struct ProjectEntity: AppEntity, Identifiable, Hashable {
    let id: String

    static let typeDisplayRepresentation: TypeDisplayRepresentation = "Project"
    static let defaultQuery = ProjectQuery()

    var displayRepresentation: DisplayRepresentation { DisplayRepresentation(title: "\(id)") }
}

struct ProjectQuery: EntityQuery {
    func entities(for identifiers: [String]) async throws -> [ProjectEntity] {
        identifiers.map(ProjectEntity.init(id:))
    }

    func suggestedEntities() async throws -> [ProjectEntity] {
        let client = try HeraldoClient(credentials: CredentialStore().load())
        let list = try await client.projects()
        return (list?.projects ?? []).map { ProjectEntity(id: $0.name) }
    }
}

enum FocusBreakThrough: String, AppEnum {
    case timeSensitive = "time-sensitive"
    case critical

    static let typeDisplayRepresentation: TypeDisplayRepresentation = "Break through"
    static let caseDisplayRepresentations: [FocusBreakThrough: DisplayRepresentation] = [
        .timeSensitive: "Time Sensitive and up",
        .critical: "Critical only",
    ]
}

struct HeraldoFocusFilter: SetFocusFilterIntent {
    static let title: LocalizedStringResource = "Set Heraldo Filter"
    static let description = IntentDescription("Chooses which heraldo alerts break through this Focus.")

    @Parameter(title: "Projects")
    var projects: [ProjectEntity]?

    @Parameter(title: "Break through", default: .timeSensitive)
    var breakThrough: FocusBreakThrough

    var displayRepresentation: DisplayRepresentation {
        let level = breakThrough == .critical ? "Critical only" : "Time Sensitive and up"
        let names = (projects ?? []).map(\.id)
        let scope = names.isEmpty ? "All projects" : names.joined(separator: ", ")
        return DisplayRepresentation(title: "\(level)", subtitle: "\(scope)")
    }

    func perform() async throws -> some IntentResult {
        let filter = FocusFilterState.from(projects: (projects ?? []).map(\.id), breakThrough: breakThrough.rawValue)
        FocusFilterStore.save(filter)
        await PushManager.shared.reportFocus()
        return .result()
    }
}

/// The filter in force on this device, in the app group's defaults.
enum FocusFilterStore {
    private static let key = "focusFilter"

    private static var defaults: UserDefaults {
        let group = Bundle.main.object(forInfoDictionaryKey: CredentialStore.accessGroupInfoKey) as? String ?? ""
        if !group.isEmpty, !group.hasPrefix("$("), let d = UserDefaults(suiteName: group) { return d }
        return .standard
    }

    static func load() -> FocusFilterState? {
        defaults.data(forKey: key).flatMap { try? JSONDecoder().decode(FocusFilterState.self, from: $0) }
    }

    static func save(_ f: FocusFilterState) {
        defaults.set(try? JSONEncoder().encode(f), forKey: key)
    }
}
