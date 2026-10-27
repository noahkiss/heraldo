import HeraldoKit
import SwiftUI
import WidgetKit

@main
struct HeraldoWidgets: WidgetBundle {
    var body: some Widget {
        StatusWidget()
        #if os(iOS)
        ProgressActivityWidget()
        #endif
    }
}

/// Placeholder home-screen widget. Stage 12 fills it from the event stream.
struct StatusWidget: Widget {
    var body: some WidgetConfiguration {
        StaticConfiguration(kind: "status", provider: StatusProvider()) { _ in
            Label(AppInfo.displayName(), systemImage: "newspaper")
                .containerBackground(.fill.tertiary, for: .widget)
        }
        .configurationDisplayName("Status")
        .description("Open events at a glance.")
    }
}

struct StatusEntry: TimelineEntry {
    let date: Date
}

struct StatusProvider: TimelineProvider {
    func placeholder(in context: Context) -> StatusEntry { StatusEntry(date: .now) }
    func getSnapshot(in context: Context, completion: @escaping (StatusEntry) -> Void) {
        completion(StatusEntry(date: .now))
    }
    func getTimeline(in context: Context, completion: @escaping (Timeline<StatusEntry>) -> Void) {
        completion(Timeline(entries: [StatusEntry(date: .now)], policy: .never))
    }
}
