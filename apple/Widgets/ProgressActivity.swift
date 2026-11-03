#if os(iOS)
import ActivityKit
import AppIntents
import HeraldoKit
import SwiftUI
import WidgetKit

/// The Live Activity for kind progress: Lock Screen, Dynamic Island, and the
/// small family the Watch Smart Stack and CarPlay show.
struct ProgressActivityWidget: Widget {
    var body: some WidgetConfiguration {
        ActivityConfiguration(for: ProgressAttributes.self) { context in
            ActivityView(state: context.state, key: context.attributes.key, stale: context.isStale)
                .widgetURL(URL(string: "heraldo://activity"))
        } dynamicIsland: { context in
            let s = context.state
            return DynamicIsland {
                DynamicIslandExpandedRegion(.leading) {
                    SymbolView(state: s).font(.title2)
                }
                DynamicIslandExpandedRegion(.trailing) {
                    TrailingText(state: s).font(.headline)
                }
                DynamicIslandExpandedRegion(.center) {
                    VStack(spacing: 2) {
                        Text(s.title ?? context.attributes.key).font(.headline).lineLimit(1)
                        if let status = s.status { Text(status).font(.caption).foregroundStyle(.secondary) }
                    }
                }
                DynamicIslandExpandedRegion(.bottom) {
                    VStack(spacing: 6) {
                        ProgressBar(state: s)
                        MetricsRow(metrics: s.shownMetrics)
                        ButtonRow(button: s.button)
                    }
                }
            } compactLeading: {
                SymbolView(state: s)
            } compactTrailing: {
                TrailingText(state: s, short: true)
            } minimal: {
                if s.fraction != nil {
                    ProgressBar(state: s).progressViewStyle(.circular)
                } else {
                    SymbolView(state: s)
                }
            }
            .keylineTint(s.color)
        }
        .supplementalActivityFamilies([.small])
    }
}

/// The Lock Screen and Watch layout.
struct ActivityView: View {
    let state: LiveState
    let key: String
    let stale: Bool
    @Environment(\.activityFamily) private var family

    var body: some View {
        Group {
            if family == .small { small } else { medium }
        }
        .opacity(stale ? 0.6 : 1)
    }

    private var small: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 4) {
                SymbolView(state: state)
                Text(state.title ?? key).font(.headline).lineLimit(1)
            }
            HStack {
                if let status = state.status { Text(status).font(.caption).lineLimit(1) }
                Spacer(minLength: 2)
                TrailingText(state: state, short: true).font(.caption)
            }
            ProgressBar(state: state)
        }
        .padding(8)
    }

    private var medium: some View {
        VStack(alignment: .leading, spacing: 8) {
            ActivityCard(state: state, key: key, stale: stale)
            ButtonRow(button: state.button)
        }
        .padding(14)
    }
}

/// The one button: open goes through the app, which opens the URL; http
/// calls the URL from the device.
struct ButtonRow: View {
    let button: LiveButton?

    var body: some View {
        if let b = button, let url = b.url {
            switch b.type {
            case "open":
                if let link = Self.openLink(url) {
                    Link(destination: link) { label(b) }
                }
            case "http":
                Button(intent: LiveButtonIntent(url: url, method: b.method)) { label(b) }
                    .buttonStyle(.bordered)
            default:
                EmptyView()
            }
        }
    }

    private func label(_ b: LiveButton) -> some View {
        Text(b.title).font(.subheadline.weight(.semibold)).frame(maxWidth: .infinity)
    }

    static func openLink(_ url: String) -> URL? {
        var c = URLComponents()
        c.scheme = "heraldo"
        c.host = "open"
        c.queryItems = [URLQueryItem(name: "url", value: url)]
        return c.url
    }
}

#Preview("Lock Screen", as: .content, using: ProgressAttributes(key: "print/42", instance: "lai_preview", source: "printer")) {
    ProgressActivityWidget()
} contentStates: {
    LiveState(title: "bracket.3mf", symbol: "printer.fill", tint: "blue", body: "PLA, 0.2 mm layers", progress: 0.42,
              endsAt: .now.addingTimeInterval(5400), status: "Printing",
              metrics: [LiveMetric(label: "Nozzle", value: "215", unit: "°C"), LiveMetric(label: "Bed", value: "60", unit: "°C")],
              button: LiveButton(id: "pause", title: "Pause", type: "http", url: "http://192.0.2.20/pause"),
              updatedAt: .now)
    LiveState(title: "bracket.3mf", symbol: "printer.fill", tint: "orange", progress: 0.42, status: "Paused")
}
#endif
