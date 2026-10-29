import HeraldoKit
import SwiftUI
import WidgetKit

@main
struct HeraldoWidgets: WidgetBundle {
    var body: some Widget {
        #if os(iOS)
        AttentionWidget()
        ProjectWidget()
        ProgressActivityWidget()
        #else
        StatusWidget()
        #endif
    }
}

#if os(macOS)
/// Placeholder Mac widget. The Mac widget extension has no app group yet, so
/// it cannot read the app's snapshot; the iOS widgets are in StatusWidgets.swift.
struct StatusWidget: Widget {
    var body: some WidgetConfiguration {
        StaticConfiguration(kind: "status", provider: PlaceholderProvider()) { _ in
            Label(AppInfo.displayName(), systemImage: "newspaper")
                .containerBackground(.fill.tertiary, for: .widget)
        }
        .configurationDisplayName("Status")
        .description("Open events at a glance.")
    }
}

struct PlaceholderEntry: TimelineEntry {
    let date: Date
}

struct PlaceholderProvider: TimelineProvider {
    func placeholder(in context: Context) -> PlaceholderEntry { PlaceholderEntry(date: .now) }
    func getSnapshot(in context: Context, completion: @escaping (PlaceholderEntry) -> Void) {
        completion(PlaceholderEntry(date: .now))
    }
    func getTimeline(in context: Context, completion: @escaping (Timeline<PlaceholderEntry>) -> Void) {
        completion(Timeline(entries: [PlaceholderEntry(date: .now)], policy: .never))
    }
}
#endif
