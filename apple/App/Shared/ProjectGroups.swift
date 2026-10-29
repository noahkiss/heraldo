import HeraldoKit
import SwiftUI

/// Items split into one group per project, for a list with a section per
/// project. Projects sort by name, and the items with none come last.
struct ProjectGroup<Item> {
    /// The project, or nil for the items with none.
    var project: String?
    var items: [Item]

    var title: String { project ?? "No project" }

    /// One group per project, in the order of `items` within each. Nil
    /// when fewer than two projects are present: the list then needs no
    /// sections by project.
    static func split(_ items: [Item], by project: (Item) -> String?) -> [ProjectGroup]? {
        var order: [String?] = []
        var groups: [String?: [Item]] = [:]
        for item in items {
            let p = project(item)
            if groups[p] == nil { order.append(p) }
            groups[p, default: []].append(item)
        }
        guard order.count > 1 else { return nil }
        let sorted = order.sorted { a, b in
            switch (a, b) {
            case (nil, _): return false
            case (_, nil): return true
            case let (a?, b?): return a.localizedCaseInsensitiveCompare(b) == .orderedAscending
            }
        }
        return sorted.map { ProjectGroup(project: $0, items: groups[$0] ?? []) }
    }
}

/// The project an event is filed under, as a small chip.
struct ProjectChip: View {
    let project: String

    var body: some View {
        Text(project)
            .font(.caption2.weight(.medium))
            .padding(.horizontal, 6)
            .padding(.vertical, 1)
            .background(.quaternary, in: Capsule())
            .foregroundStyle(.secondary)
            .accessibilityLabel(Text("Project \(project)"))
    }
}
