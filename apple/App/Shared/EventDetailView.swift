import HeraldoKit
import SwiftUI

/// One event with its full body. It fetches the event again, for the body
/// and the latest delivery outcome, and the push trace for Delivery.
struct EventDetailView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.openURL) private var openURL
    @State var event: Event
    @State private var acking = false
    @State private var ackError: String?
    @State private var trace: PushTrace?

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                header
                #if os(macOS)
                // A wide window: the actions sit under the title, above a long body.
                actions
                #endif
                if let image = event.image.flatMap(URL.init(string:)) {
                    AsyncImage(url: image) { img in
                        img.resizable().scaledToFit().clipShape(.rect(cornerRadius: 8))
                    } placeholder: {
                        ProgressView().frame(maxWidth: .infinity, minHeight: 120)
                    }
                }
                if let summary = event.summary, !summary.isEmpty, summary != event.headline {
                    Text(summary).font(.body.weight(.medium))
                }
                if let body = event.body, !body.isEmpty {
                    MarkdownView(text: body)
                }
                ForEach(event.attachmentInfo ?? []) { a in
                    AttachmentView(info: a)
                }
                #if os(iOS)
                actions
                #endif
                facts
                delivery
            }
            .padding()
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .navigationTitle(event.source)
        #if os(iOS)
        .navigationBarTitleDisplayMode(.inline)
        #endif
        .task {
            if let fresh = try? await model.client().event(id: event.id) { event = fresh }
            // Only a server with the trace sends why; older ones answer 404.
            if event.why != nil { trace = try? await model.client().why(id: event.id) }
        }
        .toolbar {
            if model.isUnread(event) {
                Button {
                    Task { await model.markRead(event) }
                } label: {
                    Label("Mark read", systemImage: "envelope.open")
                }
            }
            if let project = event.project, model.canWrite {
                Menu {
                    Button {
                        Task {
                            do { try await model.muteProject(project); ackError = nil } catch { ackError = error.localizedDescription }
                        }
                    } label: {
                        Label("Mute project for 1 h", systemImage: "bell.slash")
                    }
                } label: {
                    Label("More", systemImage: "ellipsis.circle")
                }
            }
        }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(event.headline).font(.title2.bold()).textSelection(.enabled)
            HStack(spacing: 6) {
                KindIcon(event: event)
                Text([event.kind, event.action].compactMap { $0 }.joined(separator: " · "))
                if let d = event.date {
                    Text("·")
                    Text(d, format: .dateTime.day().month().hour().minute())
                }
            }
            .font(.subheadline)
            .foregroundStyle(.secondary)
        }
    }

    @ViewBuilder
    private var actions: some View {
        HStack {
            if event.isOpenIncident {
                Button {
                    Task {
                        acking = true
                        defer { acking = false }
                        do {
                            try await model.ack(event)
                            ackError = nil
                        } catch {
                            ackError = error.localizedDescription
                        }
                    }
                } label: {
                    Label("Acknowledge", systemImage: "hand.raised")
                }
                .buttonStyle(.borderedProminent)
                .disabled(acking || model.credentials.writeToken?.isEmpty ?? true)
            }
            if let url = event.url.flatMap(URL.init(string:)) {
                Button {
                    openURL(url)
                } label: {
                    Label("Open", systemImage: "safari")
                }
                .buttonStyle(.bordered)
            }
            ForEach(event.actions ?? [], id: \.id) { a in
                if a.type == "open", let url = a.url.flatMap(URL.init(string:)) {
                    Button(a.title) { openURL(url) }.buttonStyle(.bordered)
                }
            }
            if let key = event.key, !key.isEmpty {
                NavigationLink {
                    KeyHistoryView(key: key)
                } label: {
                    Label("History", systemImage: "clock.arrow.circlepath")
                }
                .buttonStyle(.bordered)
            }
        }
        if let ackError {
            Text(ackError).font(.footnote).foregroundStyle(.red)
        }
    }

    private var facts: some View {
        VStack(alignment: .leading, spacing: 6) {
            Divider()
            fact("Source", event.source)
            fact("Host", event.host)
            fact("Project", event.project)
            fact("Key", event.key)
            fact("Level", event.level)
            fact("Thread", event.thread)
            fact("Tags", event.tags?.joined(separator: ", "))
            fact("Hold", event.hold?.label)
            if let inc = event.incident {
                fact("Incident", incidentLine(inc))
            }
            fact("ID", event.id)
        }
        .font(.footnote)
    }

    /// Why the event did or did not push: the summary, each pipeline stage,
    /// then the outcome on each device.
    @ViewBuilder
    private var delivery: some View {
        let devices = event.delivery ?? []
        if event.why != nil || !devices.isEmpty {
            VStack(alignment: .leading, spacing: 6) {
                Text("Delivery").font(.subheadline.bold())
                if let why = event.why {
                    Text(why).textSelection(.enabled)
                }
                ForEach(Array((trace?.decisions ?? []).enumerated()), id: \.offset) { _, d in
                    HStack(alignment: .firstTextBaseline) {
                        Image(systemName: d.outcome == "drop" ? "xmark.circle" : d.outcome == "pass" ? "checkmark.circle" : "arrow.down.circle")
                            .foregroundStyle(d.outcome == "drop" ? AnyShapeStyle(.orange) : AnyShapeStyle(.secondary))
                        Text(d.stage).foregroundStyle(.secondary).frame(width: 56, alignment: .leading)
                        Text(d.reason).textSelection(.enabled)
                    }
                }
                ForEach(devices, id: \.device) { d in
                    fact("Device", [d.outcome, d.level, d.error].compactMap { $0 }.joined(separator: " · "))
                }
            }
            .font(.footnote)
        }
    }

    /// "acked · opened 10:02" or "resolved · open for 12 min".
    private func incidentLine(_ inc: IncidentInfo) -> String {
        var parts = [inc.state]
        if let d = inc.openFor {
            parts.append("open for " + Duration.seconds(d).formatted(
                .units(allowed: [.days, .hours, .minutes], width: .abbreviated, maximumUnitCount: 2)))
        } else if let opened = inc.openedAt {
            parts.append("opened " + opened.formatted(date: .abbreviated, time: .shortened))
        }
        return parts.joined(separator: " · ")
    }

    @ViewBuilder
    private func fact(_ name: String, _ value: String?) -> some View {
        if let value, !value.isEmpty {
            HStack(alignment: .firstTextBaseline) {
                Text(name).foregroundStyle(.secondary).frame(width: 72, alignment: .leading)
                Text(value).textSelection(.enabled)
            }
        }
    }
}

/// Renders a markdown body block by block.
struct MarkdownView: View {
    let text: String

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(Array(Markdown.blocks(text).enumerated()), id: \.offset) { _, block in
                view(block)
            }
        }
        .textSelection(.enabled)
    }

    @ViewBuilder
    private func view(_ block: MarkdownBlock) -> some View {
        switch block {
        case .heading(let level, let t):
            inline(t).font(level == 1 ? .title3.bold() : level == 2 ? .headline : .subheadline.bold())
        case .paragraph(let t):
            inline(t)
        case .bullet(let t):
            HStack(alignment: .firstTextBaseline, spacing: 6) { Text("•"); inline(t) }
        case .numbered(let n, let t):
            HStack(alignment: .firstTextBaseline, spacing: 6) { Text("\(n).").monospacedDigit(); inline(t) }
        case .quote(let t):
            inline(t).foregroundStyle(.secondary).padding(.leading, 10)
                .overlay(alignment: .leading) { Rectangle().fill(.quaternary).frame(width: 3) }
        case .code(let t):
            ScrollView(.horizontal) {
                Text(t).font(.callout.monospaced()).padding(8)
            }
            .background(.quaternary.opacity(0.5), in: .rect(cornerRadius: 6))
        case .rule:
            Divider()
        }
    }

    private func inline(_ t: String) -> Text {
        let options = AttributedString.MarkdownParsingOptions(interpretedSyntax: .inlineOnlyPreservingWhitespace)
        if let a = try? AttributedString(markdown: t, options: options) { return Text(a) }
        return Text(t)
    }
}

/// One attached file: an image or text shown inline, any other type as its
/// type and size.
struct AttachmentView: View {
    @Environment(AppModel.self) private var model
    let info: AttachmentInfo
    @State private var data: Data?
    @State private var failed: String?

    /// Text past this many bytes is cut, so a large log stays scrollable.
    private static let textCap = 64 << 10

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            if info.isImage || info.isText {
                content
            }
            Text(caption).font(.caption).foregroundStyle(.secondary)
        }
        .task(id: info.id) {
            guard info.isImage || info.isText, data == nil else { return }
            do {
                data = try await model.client().attachment(id: info.id).data
            } catch {
                failed = error.localizedDescription
            }
        }
    }

    @ViewBuilder
    private var content: some View {
        if let data {
            if info.isImage, let image = Self.image(data) {
                image.resizable().scaledToFit().clipShape(.rect(cornerRadius: 8))
            } else if info.isText {
                ScrollView(.horizontal) {
                    Text(Self.text(data)).font(.callout.monospaced()).textSelection(.enabled).padding(8)
                }
                .background(.quaternary.opacity(0.5), in: .rect(cornerRadius: 6))
            }
        } else if failed == nil {
            ProgressView().frame(maxWidth: .infinity, minHeight: 60)
        }
    }

    private var caption: String {
        var parts = [info.contentType, ByteCountFormatter.string(fromByteCount: Int64(info.bytes), countStyle: .file)]
        if let failed { parts.append(failed) }
        return parts.joined(separator: " · ")
    }

    private static func text(_ data: Data) -> String {
        let s = String(decoding: data.prefix(textCap), as: UTF8.self)
        return data.count > textCap ? s + "\n…" : s
    }

    private static func image(_ data: Data) -> Image? {
        #if os(macOS)
        NSImage(data: data).map { Image(nsImage: $0) }
        #else
        UIImage(data: data).map { Image(uiImage: $0) }
        #endif
    }
}
