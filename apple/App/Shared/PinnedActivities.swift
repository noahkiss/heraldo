import HeraldoKit
import SwiftUI

/// The running Live Activities, pinned above the Inbox's events, each in
/// its Lock Screen layout. Shown only while one runs, and only on the
/// unfiltered list.
struct PinnedActivities<Row: View>: View {
    let activities: [ActivitySummary]
    @ViewBuilder var row: (ActivitySummary) -> Row

    var body: some View {
        if !activities.isEmpty {
            Section("Live Activities") {
                ForEach(activities) { row($0) }
            }
        }
    }
}

/// One running activity as the Lock Screen shows it.
struct ActivityRow: View {
    let activity: ActivitySummary

    var body: some View {
        ActivityCard(state: activity.content?.state ?? LiveState(title: activity.headline), key: activity.key)
            .padding(.vertical, 6)
            .accessibilityElement(children: .combine)
    }
}

/// An activity's tint as a color, the accent color by default.
func activityTint(_ s: String?) -> Color {
    LiveTint(s)?.color ?? .accentColor
}
