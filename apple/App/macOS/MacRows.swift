import HeraldoKit
import SwiftUI

/// The time on a row, as relative text in monospaced digits.
struct RowTime: View {
    let date: Date
    var prefix: String?

    var body: some View {
        Group {
            if let prefix {
                Text("\(prefix) \(date, format: .relative(presentation: .numeric, unitsStyle: .narrow))")
            } else {
                Text(date, format: .relative(presentation: .numeric, unitsStyle: .narrow))
            }
        }
        .font(.caption)
        .monospacedDigit()
        .foregroundStyle(.secondary)
        // A wrapped title never squeezes the time.
        .fixedSize()
        .help(date.formatted(date: .abbreviated, time: .shortened))
    }
}

/// One event: the kind icon in the level's color, title, summary, where it
/// came from, its project and when.
struct MacEventRow: View {
    let event: Event
    var unread = false
    var showProject = true
    var time = Time.received

    enum Time { case received, opened, openFor }

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Circle()
                .fill(unread ? AnyShapeStyle(.tint) : AnyShapeStyle(.clear))
                .frame(width: 7, height: 7)
                .padding(.top, 6)
                .accessibilityHidden(true)
            KindIcon(event: event)
                .font(.body)
                .frame(width: 22)
                .padding(.top, 1)
            VStack(alignment: .leading, spacing: 2) {
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    Text(event.headline)
                        .fontWeight(unread ? .semibold : .regular)
                        .lineLimit(2)
                    Spacer(minLength: 4)
                    trailing
                }
                if let line, !line.isEmpty, line != event.headline {
                    Text(line)
                        .font(.callout)
                        .foregroundStyle(.secondary)
                        .lineLimit(2)
                }
                HStack(spacing: 6) {
                    Text([event.source, event.host].compactMap { $0 }.joined(separator: " · "))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                    if showProject, let project = event.project {
                        ProjectChip(project: project)
                    }
                    if event.isAcked {
                        Label("Acknowledged", systemImage: "hand.raised")
                            .labelStyle(.iconOnly)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                    if let hold = event.hold, !hold.label.isEmpty {
                        Label(hold.label, systemImage: "tray.full")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .lineLimit(1)
                    } else if event.wasHeldBack {
                        Label("Not pushed", systemImage: "bell.slash")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
            }
        }
        .padding(.vertical, 3)
        .accessibilityElement(children: .combine)
        .accessibilityLabel(accessibility)
    }

    private var line: String? { event.summary ?? event.body.map(Markdown.plainText) }

    @ViewBuilder
    private var trailing: some View {
        switch time {
        case .received:
            if let d = event.date { RowTime(date: d) }
        case .opened:
            if let d = event.incident?.openedAt ?? event.date { RowTime(date: d) }
        case .openFor:
            if let d = event.incident?.openFor {
                Text("after \(Duration.seconds(d).formatted(.units(allowed: [.days, .hours, .minutes], width: .narrow, maximumUnitCount: 2)))")
                    .font(.caption)
                    .monospacedDigit()
                    .foregroundStyle(.secondary)
            }
        }
    }

    private var accessibility: Text {
        var parts: [String] = []
        if unread { parts.append("Unread") }
        if let level = event.level, level == "critical" || level == "time-sensitive" { parts.append(level) }
        parts.append(event.headline)
        if let line, !line.isEmpty, line != event.headline { parts.append(line) }
        parts.append(event.source)
        if showProject, let p = event.project { parts.append("project \(p)") }
        if event.isAcked { parts.append("acknowledged") }
        if let d = event.date { parts.append(d.formatted(.relative(presentation: .named))) }
        return Text(parts.joined(separator: ", "))
    }
}

/// One check: its state, key, host and project, next due time and last log.
struct MacCheckRow: View {
    let check: Check
    var showProject = true

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: symbol)
                .foregroundStyle(color)
                .frame(width: 22)
                .padding(.top, 1)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 2) {
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    Text(check.key).lineLimit(2)
                    Spacer(minLength: 4)
                    if check.state == .down || check.state == .late, let since = check.stateSince {
                        RowTime(date: since)
                    } else if let due = check.nextDue, check.state != .paused {
                        RowTime(date: due)
                    }
                }
                HStack(spacing: 6) {
                    // The icon carries the state's color; the text stays legible.
                    Text(check.stateLabel)
                        .font(.caption.weight(.medium))
                        .foregroundStyle(.secondary)
                    if let host = check.host {
                        Text(host).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                    }
                    if let d = check.lastDuration {
                        Label(d, systemImage: "stopwatch")
                            .font(.caption)
                            .monospacedDigit()
                            .foregroundStyle(.secondary)
                    }
                    if showProject, let p = check.project { ProjectChip(project: p) }
                }
                if let line = check.lastLogLine {
                    Text(line)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                }
            }
        }
        .padding(.vertical, 3)
        .accessibilityElement(children: .combine)
    }

    private var symbol: String {
        switch check.state {
        case .down: return "xmark.circle.fill"
        case .late: return "exclamationmark.circle.fill"
        case .started: return "play.circle.fill"
        case .up: return "checkmark.circle.fill"
        case .paused: return "pause.circle"
        case .new: return "circle.dashed"
        }
    }

    private var color: Color {
        switch check.state {
        case .up: return .green
        case .started: return .blue
        case .late: return .orange
        case .down: return .red
        case .new, .paused: return .secondary
        }
    }
}

/// One running activity: its symbol, title, status and progress.
struct MacActivityRow: View {
    let activity: ActivitySummary

    var body: some View {
        let c = activity.content
        let state = c?.state
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: c?.symbol ?? MacHome.activities.symbol)
                .foregroundStyle(tint)
                .frame(width: 22)
                .padding(.top, 1)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 3) {
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    Text(activity.headline).lineLimit(2)
                    Spacer(minLength: 4)
                    if let text = state?.progressText {
                        Text(text).font(.caption).monospacedDigit().foregroundStyle(.secondary)
                    }
                }
                if let f = state?.fraction {
                    ProgressView(value: f)
                        .progressViewStyle(.linear)
                        .tint(tint)
                        .controlSize(.small)
                        .accessibilityHidden(true)
                }
                if let line = c?.status ?? c?.body, !line.isEmpty {
                    Text(line).font(.callout).foregroundStyle(.secondary).lineLimit(1)
                }
                HStack(spacing: 6) {
                    Text(activity.source).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                    Spacer(minLength: 4)
                    if let d = activity.updatedAt { RowTime(date: d, prefix: "updated") }
                }
            }
        }
        .padding(.vertical, 3)
        .accessibilityElement(children: .combine)
    }

    private var tint: Color { activityTint(activity.content?.tint) }
}

/// An activity's tint as a color, the accent color by default.
func activityTint(_ s: String?) -> Color {
    switch LiveTint(s) {
    case .named(let name):
        switch name {
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
    case .rgba(let r, let g, let b, let a): return Color(red: r, green: g, blue: b, opacity: a)
    case nil: return .accentColor
    }
}

/// One held alert: its title, source and when it forwards.
struct MacHeldRow: View {
    let entry: HoldEntry

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: MacHome.held.symbol)
                .foregroundStyle(.orange)
                .frame(width: 22)
                .padding(.top, 1)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 2) {
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    Text(entry.headline).lineLimit(2)
                    Spacer(minLength: 4)
                    RowTime(date: entry.date)
                }
                if let body = entry.body, !body.isEmpty, body != entry.headline {
                    Text(body).font(.callout).foregroundStyle(.secondary).lineLimit(2)
                }
                HStack(spacing: 6) {
                    if let s = entry.source ?? entry.client {
                        Text(s).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                    }
                    if entry.shadow == true {
                        Text("Shadow").font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer(minLength: 4)
                    if let d = entry.deadline, d > .now { RowTime(date: d, prefix: "forwards") }
                }
            }
        }
        .padding(.vertical, 3)
        .accessibilityElement(children: .combine)
    }
}
