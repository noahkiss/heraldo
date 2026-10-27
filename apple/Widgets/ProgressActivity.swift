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
                        if let f = s.fraction { ProgressView(value: f).tint(s.color) }
                        MetricsRow(metrics: s.shownMetrics)
                        ButtonRow(button: s.button)
                    }
                }
            } compactLeading: {
                SymbolView(state: s)
            } compactTrailing: {
                TrailingText(state: s, short: true)
            } minimal: {
                if let f = s.fraction {
                    ProgressView(value: f).progressViewStyle(.circular).tint(s.color)
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
            if let f = state.fraction { ProgressView(value: f).tint(state.color) }
        }
        .padding(8)
    }

    private var medium: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                SymbolView(state: state).font(.title3)
                VStack(alignment: .leading, spacing: 1) {
                    Text(state.title ?? key).font(.headline).lineLimit(1)
                    if let status = state.status {
                        Text(stale ? "\(status) · not updated" : status)
                            .font(.subheadline).foregroundStyle(.secondary).lineLimit(1)
                    }
                }
                Spacer(minLength: 4)
                TrailingText(state: state).font(.headline)
            }
            if !state.shownMetrics.isEmpty {
                MetricsRow(metrics: state.shownMetrics)
            } else if let body = state.body {
                Text(body).font(.footnote).foregroundStyle(.secondary).lineLimit(2)
            }
            if let f = state.fraction {
                ProgressView(value: f).tint(state.color)
            }
            ButtonRow(button: state.button)
        }
        .padding(14)
    }
}

/// The trailing text: a running countdown, else the trailing text, else
/// the progress.
struct TrailingText: View {
    let state: LiveState
    var short = false

    var body: some View {
        if let range = state.countdown(at: .now) {
            Text(timerInterval: range, countsDown: true)
                .monospacedDigit()
                .multilineTextAlignment(.trailing)
                .frame(maxWidth: short ? 52 : 80, alignment: .trailing)
        } else if let t = state.trailing {
            Text(t).lineLimit(1)
        } else if let p = state.progressText {
            Text(p).monospacedDigit().lineLimit(1)
        }
    }
}

struct SymbolView: View {
    let state: LiveState

    var body: some View {
        Image(systemName: state.symbol ?? "gauge.with.dots.needle.33percent")
            .foregroundStyle(state.color)
    }
}

struct MetricsRow: View {
    let metrics: [LiveMetric]

    var body: some View {
        if !metrics.isEmpty {
            HStack(spacing: 6) {
                ForEach(metrics, id: \.self) { m in
                    VStack(spacing: 0) {
                        Text(m.text).font(.caption.weight(.semibold)).monospacedDigit().lineLimit(1)
                        Text(m.label).font(.caption2).foregroundStyle(.secondary).lineLimit(1)
                    }
                    .foregroundStyle(LiveTint(m.color)?.color ?? .primary)
                    .frame(maxWidth: .infinity)
                }
            }
        }
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

extension LiveState {
    var color: Color { LiveTint(tint)?.color ?? .accentColor }
}

extension LiveTint {
    var color: Color {
        switch self {
        case .rgba(let r, let g, let b, let a):
            return Color(.sRGB, red: r, green: g, blue: b, opacity: a)
        case .named(let n):
            switch n {
            case "red": return .red
            case "orange": return .orange
            case "yellow": return .yellow
            case "green": return .green
            case "mint": return .mint
            case "teal": return .teal
            case "cyan": return .cyan
            case "blue": return .blue
            case "indigo": return .indigo
            case "purple": return .purple
            case "pink": return .pink
            case "brown": return .brown
            case "gray": return .gray
            case "secondary": return .secondary
            default: return .primary
            }
        }
    }
}

#Preview("Lock Screen", as: .content, using: ProgressAttributes(key: "print/42", instance: "lai_preview", source: "printer")) {
    ProgressActivityWidget()
} contentStates: {
    LiveState(title: "bracket.3mf", symbol: "printer.fill", tint: "blue", body: "PLA, 0.2 mm layers", progress: 0.42,
              endsAt: .now.addingTimeInterval(5400), status: "Printing",
              metrics: [LiveMetric(label: "Nozzle", value: "215", unit: "°C"), LiveMetric(label: "Bed", value: "60", unit: "°C")],
              button: LiveButton(id: "pause", title: "Pause", type: "http", url: "http://192.0.2.20/pause"))
    LiveState(title: "bracket.3mf", symbol: "printer.fill", tint: "orange", progress: 0.42, status: "Paused")
}
#endif
