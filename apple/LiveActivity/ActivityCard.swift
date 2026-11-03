import HeraldoKit
import SwiftUI

/// The Lock Screen layout of a progress Live Activity: symbol, title,
/// status, countdown, metrics or body, and the progress bar. The widget
/// shows it on the Lock Screen; the app pins it to the top of the Inbox.
/// Compiled into the app and the widget extension.
struct ActivityCard: View {
    let state: LiveState
    let key: String
    var stale = false

    var body: some View {
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
            ProgressBar(state: state)
        }
    }
}

/// The progress bar, in the activity's tint. With an end time and the time
/// the fraction was true, a local timer moves it between pushes, and each push
/// re-anchors it. Without them it draws the fraction as pushed. Nothing
/// without a fraction.
struct ProgressBar: View {
    let state: LiveState

    var body: some View {
        Group {
            if let range = state.progressTimer(at: .now) {
                ProgressView(timerInterval: range, countsDown: false) {
                    EmptyView()
                } currentValueLabel: {
                    EmptyView()
                }
            } else if let f = state.fraction {
                ProgressView(value: f)
            }
        }
        .tint(state.color)
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
