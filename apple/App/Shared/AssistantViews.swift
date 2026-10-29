import HeraldoKit
import SwiftUI

/// A toolbar button that opens the Ask sheet. It shows only while the
/// server's assistant takes questions.
struct AskButton: View {
    @Environment(AppModel.self) private var model
    let scope: AskScope
    /// Applies a filter proposal to the list the sheet was opened from. Nil
    /// hides Apply for filters.
    var onFilter: ((ViewFilter) -> Void)?
    @State private var showing = false

    var body: some View {
        if model.chatAvailable {
            Button {
                showing = true
            } label: {
                Label("Ask", systemImage: "sparkles")
            }
            .help("Ask")
            .sheet(isPresented: $showing) {
                AskSheet(scope: scope, onFilter: onFilter)
                    .environment(model)
            }
        }
    }
}

/// A question to the server's assistant, its answer, the events it cites,
/// and Apply for a suggested change.
struct AskSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let scope: AskScope
    var onFilter: ((ViewFilter) -> Void)?
    @State private var question = ""
    @State private var asking = false
    @State private var answer: AskAnswer?
    @State private var cited: [Event] = []
    @State private var error: String?
    @State private var applying = false
    @State private var applied: String?

    private var trimmed: String { question.trimmingCharacters(in: .whitespacesAndNewlines) }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("Question", text: $question, axis: .vertical)
                        .lineLimit(1...5)
                        .onSubmit { Task { await ask() } }
                    Button {
                        Task { await ask() }
                    } label: {
                        if asking { ProgressView() } else { Text("Ask") }
                    }
                    .disabled(asking || trimmed.isEmpty)
                } footer: {
                    if let s = scopeText { Text(s) }
                }
                if let error {
                    Section { Text(error).foregroundStyle(.red) }
                }
                if let answer {
                    Section("Answer") {
                        Text(answer.answer).textSelection(.enabled)
                    }
                    if !cited.isEmpty {
                        Section("Events") {
                            ForEach(cited) { e in
                                NavigationLink(value: e) { EventRow(event: e) }
                            }
                        }
                    }
                    if let p = answer.proposal {
                        Section("Suggestion") {
                            Text(p.summary ?? p.description)
                            if p.summary != nil {
                                Text(p.description).font(.caption).foregroundStyle(.secondary)
                            }
                            if let applied {
                                Label(applied, systemImage: "checkmark").foregroundStyle(.secondary)
                            } else {
                                Button("Apply") { Task { await apply(p) } }
                                    .disabled(applying || !canApply(p))
                            }
                        }
                    }
                }
            }
            .formStyle(.grouped)
            .navigationTitle("Ask")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .navigationDestination(for: Event.self) { EventDetailView(event: $0) }
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Done") { dismiss() }
                }
            }
        }
        #if os(macOS)
        .frame(minWidth: 460, minHeight: 420)
        #endif
    }

    private var scopeText: String? {
        var parts: [String] = []
        if let v = scope.view {
            parts.append("View: \(model.views.first { $0.id == v }?.name ?? v)")
        }
        if let p = scope.project { parts.append("Project: \(p)") }
        if let k = scope.key { parts.append("Key: \(k)") }
        return parts.isEmpty ? nil : parts.joined(separator: " · ")
    }

    private func canApply(_ p: AskProposal) -> Bool {
        switch p.type {
        case "filter": return p.filter != nil && onFilter != nil
        case "view": return p.view != nil && model.canWrite
        case "guidance": return p.guidance != nil && model.canWrite
        case "mute": return p.mute != nil && model.canWrite
        default: return false
        }
    }

    private func ask() async {
        guard !trimmed.isEmpty, !asking else { return }
        asking = true
        defer { asking = false }
        error = nil
        applied = nil
        do {
            let client = try model.client()
            let a = try await client.ask(AskInput(question: trimmed, scope: scope))
            answer = a
            var events: [Event] = []
            for id in a.citedIDs {
                if let e = model.cache.events.first(where: { $0.id == id }) {
                    events.append(e)
                } else if let e = try? await client.event(id: id) {
                    events.append(e)
                }
            }
            cited = events
        } catch let e as HeraldoError where e.status == 409 {
            error = "The assistant is off on the server."
        } catch {
            self.error = error.localizedDescription
        }
    }

    /// Applies a proposal with the ordinary write call.
    private func apply(_ p: AskProposal) async {
        applying = true
        defer { applying = false }
        do {
            let client = try model.client()
            switch p.type {
            case "filter":
                if let f = p.filter { onFilter?(f) }
                dismiss()
                return
            case "view":
                if let v = p.view {
                    try await client.createView(v)
                    await model.reloadViews()
                    applied = "Saved view \(v.name)"
                }
            case "guidance":
                if let g = p.guidance {
                    try await client.addGuidance(g)
                    applied = "Added guidance"
                }
            case "mute":
                if let m = p.mute {
                    try await client.mute(m)
                    applied = "Muted"
                }
            default:
                break
            }
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }
}

extension AskProposal {
    /// What applying the proposal does, in one line.
    var description: String {
        switch type {
        case "filter":
            return "Filter the list: " + (filter.map(Self.filterText) ?? "")
        case "view":
            return "Save view \(view?.name ?? "")" + (view.map { ": " + Self.filterText($0.filter) } ?? "")
        case "guidance":
            return "Add guidance for \(guidance?.scope ?? ""): \(guidance?.text ?? "")"
        case "mute":
            let target = [mute?.key, mute?.project, mute?.source].compactMap { $0 }.joined(separator: ", ")
            return "Mute \(target) for \(mute?.for ?? "")"
        default:
            return type
        }
    }

    static func filterText(_ f: ViewFilter) -> String {
        var parts: [String] = []
        if let p = f.project { parts.append("project \(p.joined(separator: ", "))") }
        if let s = f.source { parts.append("source \(s)") }
        if let k = f.kind { parts.append("kind \(k)") }
        if let h = f.host { parts.append("host \(h)") }
        if let t = f.tag { parts.append("tag \(t)") }
        if let l = f.level { parts.append("level \(l.joined(separator: ", "))") }
        if let q = f.q { parts.append("text \"\(q)\"") }
        return parts.isEmpty ? "every event" : parts.joined(separator: ", ")
    }
}

extension InboxFeed {
    /// The Ask sheet's scope: the saved view and the project shown.
    var askScope: AskScope {
        AskScope(view: viewID, project: project == noProject ? nil : project)
    }

    /// Applies a filter proposal: project, source, kind and text. The
    /// feed has no host, tag or level filter; those fields are left out.
    func apply(_ f: ViewFilter) {
        if let p = f.project?.first { project = p }
        source = f.source
        kind = f.kind
        query = f.q ?? ""
    }
}

/// Settings: the server's guidance lines for the assistant. Everyone reads
/// them; the write token adds, turns off and removes them.
struct GuidanceSection: View {
    @Environment(AppModel.self) private var model
    @State private var lines: [Guidance] = []
    @State private var supported = false
    @State private var scope = "global"
    @State private var text = ""
    @State private var error: String?

    var body: some View {
        Group {
            if supported { section }
        }
        .task(id: model.lastSync) { await load() }
    }

    private var section: some View {
        Section {
            if lines.isEmpty {
                Text("None").foregroundStyle(.secondary)
            }
            ForEach(lines) { g in
                HStack {
                    VStack(alignment: .leading) {
                        Text(g.text).foregroundStyle(g.enabled ? .primary : .secondary)
                        Text(g.scope).font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer()
                    if model.canWrite {
                        Toggle("On", isOn: Binding(get: { g.enabled }, set: { on in Task { await setEnabled(g, on) } }))
                            .labelsHidden()
                        Button(role: .destructive) {
                            Task { await delete(g) }
                        } label: {
                            Label("Delete", systemImage: "trash")
                        }
                        .labelStyle(.iconOnly)
                        .buttonStyle(.borderless)
                    }
                }
            }
            if model.canWrite {
                TextField("Scope", text: $scope)
                    .autocorrectionDisabled()
                    #if os(iOS)
                    .textInputAutocapitalization(.never)
                    #endif
                TextField("Guidance", text: $text, axis: .vertical)
                    .lineLimit(1...4)
                Button("Add") { Task { await add() } }
                    .disabled(text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
                        || scope.trimmingCharacters(in: .whitespaces).isEmpty)
            }
            if let error {
                Text(error).font(.footnote).foregroundStyle(.red)
            }
        } header: {
            Text("Guidance")
        } footer: {
            Text("Scope: global, project:<name>, source:<name> or key:<regex>.")
        }
    }

    private func load() async {
        guard model.isConfigured, let client = try? model.client() else { return }
        do {
            let list = try await client.guidance()
            supported = list != nil
            lines = list ?? []
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func add() async {
        do {
            try await model.client().addGuidance(GuidanceInput(scope: scope.trimmingCharacters(in: .whitespaces),
                                                               text: text.trimmingCharacters(in: .whitespacesAndNewlines)))
            text = ""
            await load()
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func setEnabled(_ g: Guidance, _ on: Bool) async {
        do {
            try await model.client().updateGuidance(id: g.id, GuidancePatch(enabled: on))
            await load()
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func delete(_ g: Guidance) async {
        do {
            try await model.client().deleteGuidance(id: g.id)
            await load()
        } catch {
            self.error = error.localizedDescription
        }
    }
}
