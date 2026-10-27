import HeraldoKit
import SwiftUI

/// Every check: its state, when the next ping is due, the last run's length
/// and the last log line.
struct CheckinsView: View {
    @Environment(AppModel.self) private var model
    @State private var error: String?

    var body: some View {
        NavigationStack {
            List {
                if let error {
                    Text(error).font(.footnote).foregroundStyle(.red)
                }
                ForEach(model.checks) { c in
                    NavigationLink(value: c) { CheckRow(check: c) }
                        .swipeActions {
                            if !(model.credentials.writeToken?.isEmpty ?? true) {
                                let pause = c.state != .paused
                                Button {
                                    Task {
                                        do { try await model.setPaused(c, pause); error = nil } catch { self.error = error.localizedDescription }
                                    }
                                } label: {
                                    Label(pause ? "Pause" : "Resume", systemImage: pause ? "pause.circle" : "play.circle")
                                }
                                .tint(pause ? .gray : .blue)
                            }
                        }
                }
            }
            .listStyle(.plain)
            .navigationTitle("Check-ins")
            .navigationDestination(for: Check.self) { c in
                CheckDetailView(key: c.key)
            }
            .refreshable { await model.refresh() }
            .overlay {
                if model.checks.isEmpty {
                    ContentUnavailableView("No check-ins", systemImage: "clock.badge.checkmark")
                }
            }
        }
    }
}

struct CheckRow: View {
    let check: Check

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack(alignment: .firstTextBaseline) {
                Text(check.key).font(.headline).lineLimit(1)
                Spacer(minLength: 4)
                CheckStateBadge(check: check)
            }
            HStack(spacing: 10) {
                if let due = check.nextDue, check.state != .paused, check.state != .down {
                    Label { Text(due, format: .relative(presentation: .named, unitsStyle: .narrow)) } icon: { Image(systemName: "clock") }
                }
                if let d = check.lastDuration {
                    Label(d, systemImage: "stopwatch")
                }
                if let host = check.host {
                    Text(host)
                }
            }
            .font(.caption)
            .foregroundStyle(.secondary)
            if let line = check.lastLogLine {
                Text(line)
                    .font(.caption.monospaced())
                    .foregroundStyle(.tertiary)
                    .lineLimit(1)
            }
        }
        .padding(.vertical, 2)
    }
}

struct CheckStateBadge: View {
    let check: Check

    var body: some View {
        Text(check.stateLabel)
            .font(.caption.weight(.semibold))
            .padding(.horizontal, 6)
            .padding(.vertical, 2)
            .background(color.opacity(0.18), in: Capsule())
            .foregroundStyle(color)
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

/// One check, with its schedule, times and the full last log.
struct CheckDetailView: View {
    @Environment(AppModel.self) private var model
    let key: String

    var body: some View {
        if let c = model.checks.first(where: { $0.key == key }) {
            List {
                Section {
                    LabeledContent("State") { CheckStateBadge(check: c) }
                    if let since = c.stateSince { row("Since", since) }
                    if let host = c.host { LabeledContent("Host", value: host) }
                    if let s = c.schedule { LabeledContent("Schedule", value: s.summary) }
                }
                Section {
                    if let d = c.nextDue { row("Next due", d) }
                    if let d = c.downAfter, c.state != .down { row("Down after", d) }
                    if let d = c.startedAt { row("Running since", d) }
                    if let d = c.lastPing { row("Last ping", d) }
                    if let d = c.lastSuccess { row("Last success", d) }
                    if let d = c.lastDuration { LabeledContent("Last run", value: d) }
                }
                if let log = c.lastLog, !log.isEmpty {
                    Section {
                        Text(log)
                            .font(.caption.monospaced())
                            .textSelection(.enabled)
                    } header: {
                        if let at = c.lastLogAt {
                            Text("Last log, \(at.formatted(date: .abbreviated, time: .shortened))")
                        } else {
                            Text("Last log")
                        }
                    }
                }
            }
            .navigationTitle(c.key)
        } else {
            ContentUnavailableView("Check not found", systemImage: "questionmark.circle")
        }
    }

    private func row(_ label: String, _ date: Date) -> some View {
        LabeledContent(label) {
            Text(date.formatted(date: .abbreviated, time: .shortened))
        }
    }
}
