import HeraldoKit
import SwiftUI

/// One event with its full body. It fetches the event again, for the body
/// and the latest delivery outcome.
struct EventDetailView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.openURL) private var openURL
    @State var event: Event
    @State private var acking = false
    @State private var ackError: String?

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                header
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
                actions
                facts
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
        }
        .toolbar {
            if model.isUnread(event) {
                Button {
                    Task { await model.markRead(event) }
                } label: {
                    Label("Mark read", systemImage: "envelope.open")
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
            fact("Key", event.key)
            fact("Level", event.level)
            fact("Thread", event.thread)
            fact("Tags", event.tags?.joined(separator: ", "))
            fact("Hold", event.hold?.state)
            ForEach(event.delivery ?? [], id: \.device) { d in
                fact("Delivery", [d.outcome, d.level, d.error].compactMap { $0 }.joined(separator: " · "))
            }
            fact("ID", event.id)
        }
        .font(.footnote)
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
