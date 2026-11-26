import HeraldoKit
import Observation
import SwiftUI

/// The delivery settings and sources the Settings screens show: one copy,
/// shared by the rows list, the editor and the sources list. Reads use the
/// read token; writes need the admin token, as server settings do.
@MainActor
@Observable
final class DeliveryStore {
    private(set) var list: DeliverySettingList?
    private(set) var sources: [Source] = []
    /// `delivery.enabled` and `delivery.new_source_level` as server
    /// settings, for the lock a flag or env var sets.
    private(set) var settings: [String: ServerSetting] = [:]
    /// The server answers `/v1/delivery`.
    private(set) var supported = true
    private(set) var loaded = false
    var error: String?

    var rows: [DeliverySetting] { list?.settings ?? [] }
    var newSources: Int { sources.filter(\.new).count }

    func load(_ model: AppModel) async {
        guard model.isConfigured, let client = try? model.client() else { return }
        do {
            let list = try await client.deliverySettings()
            supported = list != nil
            self.list = list
            if list != nil {
                sources = Source.ordered(try await client.sources() ?? [])
                let all = try await client.settings() ?? []
                settings = Dictionary(uniqueKeysWithValues: all.filter { $0.key.hasPrefix("delivery.") }.map { ($0.key, $0) })
            }
            loaded = true
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }

    /// Runs one admin write, then reloads the rows and sources and the
    /// badge, which a row change moves. Returns whether it worked.
    @discardableResult
    func write(_ model: AppModel, _ body: (HeraldoClient) async throws -> Void) async -> Bool {
        do {
            try await body(model.client())
            error = nil
            await load(model)
            await model.refresh([.unread])
            return true
        } catch {
            // The reload clears the error when it works; keep the write's.
            let message = error.localizedDescription
            await load(model)
            self.error = message
            return false
        }
    }

    /// The position after every row: a new row goes last.
    var nextPosition: Int { min((rows.map(\.position).max() ?? -1) + 1, 10000) }

    /// Moves the rows to a new order: each row whose position differs from
    /// its place is written again, so ties from equal positions go too.
    func reorder(_ model: AppModel, _ ordered: [DeliverySetting]) async {
        await write(model) { client in
            for (i, row) in ordered.enumerated() where row.position != i {
                var input = DeliverySettingInput(row)
                input.position = i
                try await client.updateDeliverySetting(id: row.id, input)
            }
        }
    }
}

/// Settings' entry to delivery: whether the rows act, and links to the
/// rows and the sources.
struct DeliverySection: View {
    @Environment(AppModel.self) private var model
    @State private var store = DeliveryStore()

    var body: some View {
        Section {
            if store.loaded && !store.supported {
                Text("This server has no delivery settings.").foregroundStyle(.secondary)
            } else {
                NavigationLink {
                    DeliverySettingsView(store: store)
                } label: {
                    LabeledContent("Delivery settings") {
                        if let list = store.list {
                            Text(list.enabled ? (list.settings.count == 1 ? "1 row" : "\(list.settings.count) rows") : "Off")
                        }
                    }
                }
                NavigationLink {
                    SourcesView(store: store)
                } label: {
                    LabeledContent("Sources") {
                        if store.newSources > 0 { Text("\(store.newSources) new") }
                    }
                }
            }
        } header: {
            Text("Delivery")
        } footer: {
            Text("Rows decide, per source, tag or level, whether an event alerts, arrives silently, waits for the digest or is dropped, and whether it counts toward the badge.")
        }
        .disabled(!model.isConfigured)
        .task(id: model.credentials) { await store.load(model) }
    }
}

/// The delivery rows in order, with the switch that turns them on.
struct DeliverySettingsView: View {
    @Environment(AppModel.self) private var model
    @Bindable var store: DeliveryStore
    @State private var editing: EditTarget?
    @State private var deleting: DeliverySetting?

    private var canEdit: Bool { !(model.credentials.adminToken ?? "").isEmpty }

    var body: some View {
        Form {
            Section {
                Toggle("Use delivery settings", isOn: Binding(
                    get: { store.list?.enabled ?? false },
                    set: { on in Task { await store.write(model) { try await $0.putSetting("delivery.enabled", .bool(on)) } } }
                ))
                .disabled(!editable("delivery.enabled"))
                Picker("New source without a level", selection: Binding(
                    get: { store.list?.newSourceLevel ?? "passive" },
                    set: { v in Task { await store.write(model) { try await $0.putSetting("delivery.new_source_level", .string(v)) } } }
                )) {
                    Text("Passive").tag("passive")
                    Text("Active").tag("active")
                }
                .disabled(!editable("delivery.new_source_level"))
            } footer: {
                Text("Off, the rows do nothing. A source stays new until you review it in Sources.")
            }

            Section {
                if store.rows.isEmpty {
                    Text(store.loaded ? "No rows" : "Loading").foregroundStyle(.secondary)
                }
                ForEach(store.rows) { row in
                    Button {
                        editing = .row(row)
                    } label: {
                        DeliveryRowLabel(row: row)
                    }
                    .buttonStyle(.plain)
                    .contextMenu { menu(row) }
                    .swipeActions {
                        if canEdit {
                            Button("Delete", role: .destructive) { deleting = row }
                        }
                    }
                }
                .onMove(perform: canEdit ? move : nil)
                Button {
                    editing = .new(DeliverySettingInput(position: store.nextPosition))
                } label: {
                    Label("Add Row", systemImage: "plus")
                }
                .disabled(!canEdit)
            } header: {
                Text("Rows")
            } footer: {
                Text(canEdit ? "The first row that matches an event decides. Incidents, time-sensitive events, failing check-ins and heraldo's own events always alert; a row can still take them out of the badge."
                     : "Read-only without an admin token.")
            }

            if let error = store.error {
                Section { Text(error).font(.footnote).foregroundStyle(.red) }
            }
        }
        .formStyle(.grouped)
        .navigationTitle("Delivery Settings")
        .refreshable { await store.load(model) }
        .task { await store.load(model) }
        .sheet(item: $editing) { target in
            DeliveryRowEditor(store: store, target: target)
        }
        .confirmationDialog("Delete this row?", isPresented: Binding(
            get: { deleting != nil }, set: { if !$0 { deleting = nil } }
        ), presenting: deleting) { row in
            Button("Delete", role: .destructive) {
                Task { await store.write(model) { try await $0.deleteDeliverySetting(id: row.id) } }
            }
        } message: { row in
            Text("Events that match \(row.match.summary.lowercased()) go back to the next row, or to the defaults.")
        }
    }

    @ViewBuilder
    private func menu(_ row: DeliverySetting) -> some View {
        let i = store.rows.firstIndex(of: row) ?? 0
        Button("Edit…") { editing = .row(row) }
        Button("Move Up") { move(from: IndexSet(integer: i), to: i - 1) }
            .disabled(!canEdit || i == 0)
        Button("Move Down") { move(from: IndexSet(integer: i), to: i + 2) }
            .disabled(!canEdit || i == store.rows.count - 1)
        Divider()
        Button("Delete…", role: .destructive) { deleting = row }
            .disabled(!canEdit)
    }

    private func move(from: IndexSet, to: Int) {
        var ordered = store.rows
        ordered.move(fromOffsets: from, toOffset: max(0, min(to, ordered.count)))
        Task { await store.reorder(model, ordered) }
    }

    private func editable(_ key: String) -> Bool {
        canEdit && store.list != nil && !(store.settings[key]?.isLocked ?? false)
    }
}

/// A row in the list: its note or match, then what it does.
struct DeliveryRowLabel: View {
    let row: DeliverySetting

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title).lineLimit(1)
            if row.note?.isEmpty == false {
                Text(row.match.summary).font(.caption).foregroundStyle(.secondary).lineLimit(1)
            }
            Label(row.effect, systemImage: symbol)
                .font(.caption)
                .foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .contentShape(Rectangle())
    }

    private var title: String {
        if let note = row.note, !note.isEmpty { return note }
        return row.match.summary
    }

    private var symbol: String {
        switch row.deliver {
        case .banner?: return "bell"
        case .silent?: return "bell.slash"
        case .digest?: return "list.bullet.rectangle"
        case .drop?: return "trash"
        case nil: return row.badge == .auto ? "arrow.down.to.line" : "app.badge"
        }
    }
}

/// What the editor sheet edits: a stored row, or a new one.
enum EditTarget: Identifiable, Hashable {
    case row(DeliverySetting)
    case new(DeliverySettingInput)

    var id: String {
        switch self {
        case .row(let r): return r.id
        case .new: return "new"
        }
    }
}

/// One row's editor, with a live preview of what the whole list would do
/// with the edit over the last 30 days.
struct DeliveryRowEditor: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let store: DeliveryStore
    let target: EditTarget
    @State private var input: DeliverySettingInput
    @State private var preview: DeliveryPreview?
    @State private var previewError: String?
    @State private var saving = false
    @State private var saveError: String?

    init(store: DeliveryStore, target: EditTarget) {
        self.store = store
        self.target = target
        switch target {
        case .row(let r): _input = State(initialValue: DeliverySettingInput(r))
        case .new(let i): _input = State(initialValue: i)
        }
    }

    private var canEdit: Bool { !(model.credentials.adminToken ?? "").isEmpty }

    private var original: DeliverySettingInput {
        switch target {
        case .row(let r): return DeliverySettingInput(r)
        case .new(let i): return i
        }
    }

    private var changed: Bool { input.normalized != original.normalized }

    /// The list the preview runs: every stored row in order, with this one
    /// edited in its place, or added where its position puts it.
    private var proposed: (rows: [DeliverySettingInput], index: Int) {
        var mine = DeliverySetting(id: "__edit", match: input.match, deliver: input.deliver, badge: input.badge ?? .auto,
                                   defaultLevel: input.defaultLevel, position: input.position ?? 0, note: input.note)
        // The server orders by position, then by age: an edited row keeps
        // its age, and a new one is the youngest.
        var items = store.rows.enumerated().map { (row: $1, age: $0) }
        var age = Int.max
        if case .row(let r) = target, let i = items.firstIndex(where: { $0.row.id == r.id }) {
            age = items[i].age
            mine.id = r.id
            items.remove(at: i)
        }
        items.append((mine, age))
        items.sort { ($0.row.position, $0.age) < ($1.row.position, $1.age) }
        let index = items.firstIndex { $0.row.id == mine.id } ?? 0
        return (items.map { DeliverySettingInput($0.row) }, index)
    }

    var body: some View {
        NavigationStack {
            Form {
                matchSection
                outcomeSection
                Section("Note") {
                    TextField("What this row is for", text: Binding(get: { input.note ?? "" }, set: { input.note = $0 }))
                }
                previewSection
                if let problem = input.problem ?? saveError {
                    Section { Text(problem).font(.footnote).foregroundStyle(.red) }
                }
                if case .row(let r) = target, canEdit {
                    Section {
                        Button("Delete Row", role: .destructive) {
                            Task {
                                if await store.write(model, { try await $0.deleteDeliverySetting(id: r.id) }) { dismiss() }
                            }
                        }
                    }
                }
            }
            .formStyle(.grouped)
            .disabled(!canEdit || saving)
            .navigationTitle(isNew ? "New Row" : "Edit Row")
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Save") { Task { await save() } }
                        .disabled(!canEdit || saving || input.problem != nil || (!isNew && !changed))
                }
            }
            .interactiveDismissDisabled(changed)
            .task(id: input) {
                // Wait for typing to settle, then ask the server.
                try? await Task.sleep(for: .milliseconds(500))
                guard !Task.isCancelled else { return }
                await runPreview()
            }
        }
        #if os(macOS)
        .frame(minWidth: 460, minHeight: 560)
        #endif
    }

    private var isNew: Bool { if case .new = target { return true } else { return false } }

    // MARK: Sections

    private var matchSection: some View {
        Section {
            field("Source", \.source, prompt: "Any source")
            field("Tag", \.tag, prompt: "Any tag")
            Picker("Kind", selection: Binding(get: { input.match.kind }, set: { input.match.kind = $0 })) {
                Text("Any kind").tag(String?.none)
                ForEach(Kind.allCases.filter { $0 != .progress && $0 != .status }, id: \.self) {
                    Text($0.rawValue.capitalized).tag(String?.some($0.rawValue))
                }
            }
            LabeledContent("Level") {
                HStack(spacing: 6) {
                    ForEach(Self.levels, id: \.self) { level in
                        Toggle(ServerSettingsSection.levelNames[level] ?? level, isOn: levelBinding(level))
                            .toggleStyle(.button)
                            .controlSize(.small)
                    }
                }
            }
            field("Project", \.project, prompt: "Any project")
            field("Ingest rule", \.rule, prompt: "Any rule")
            field("Key prefix", \.key, prompt: "Any key, such as checkin:")
        } header: {
            Text("Match")
        } footer: {
            Text("Every field set must match. Source and tag ignore case. An event without a level counts as active.")
        }
    }

    private var outcomeSection: some View {
        Section {
            Picker("Deliver", selection: $input.deliver) {
                Text("No change").tag(DeliverOutcome?.none)
                ForEach(DeliverOutcome.allCases, id: \.self) { Text($0.label).tag(DeliverOutcome?.some($0)) }
            }
            Picker("Badge", selection: Binding(get: { input.badge ?? .auto }, set: { input.badge = $0 })) {
                Text("Auto").tag(DeliveryBadge.auto)
                Text("Counts").tag(DeliveryBadge.on)
                Text("Never counts").tag(DeliveryBadge.off)
            }
            Picker("Level when missing", selection: Binding(get: { input.defaultLevel }, set: { input.defaultLevel = $0 })) {
                Text("No change").tag(String?.none)
                Text("Passive").tag(String?.some("passive"))
                Text("Active").tag(String?.some("active"))
            }
        } header: {
            Text("Outcome")
        } footer: {
            Text(outcomeHelp)
        }
    }

    private var outcomeHelp: String {
        var s: String
        switch input.deliver {
        case .banner?: s = "Banner: delivered at the event's own level."
        case .silent?: s = "Silent: in the inbox, no notification."
        case .digest?: s = "Digest: out of the inbox, listed once in the digest. While the digest is off, it acts as silent."
        case .drop?: s = "Drop: stored and searchable, never shown or pushed."
        case nil: s = "No change to how the event is delivered."
        }
        s += " Badge Auto follows the badge setting; Counts and Never counts apply to incidents too."
        return s
    }

    @ViewBuilder
    private var previewSection: some View {
        Section {
            if let p = preview {
                let mine = p.row(proposed.index)
                LabeledContent("This row matches", value: "\(mine?.hits ?? 0) of \(p.events)")
                if let refused = mine?.refused, refused > 0 {
                    LabeledContent("Kept loud by a floor", value: "\(refused)")
                }
                LabeledContent("Silent · Digest · Drop", value: "\(p.outcomes.silent) · \(p.outcomes.digest) · \(p.outcomes.drop)")
                LabeledContent("Badge", value: "+\(p.badgeJoined) join, −\(p.badgeLeft) leave")
                let changes = p.changed.filter { $0.row == proposed.index }.prefix(8)
                ForEach(changes) { c in
                    VStack(alignment: .leading, spacing: 1) {
                        Text(c.title ?? c.source).lineLimit(1)
                        Text(changeLine(c)).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                    }
                }
            } else if let previewError {
                Text(previewError).font(.footnote).foregroundStyle(.secondary)
            } else {
                ProgressView().frame(maxWidth: .infinity)
            }
        } header: {
            Text("Preview: last 30 days")
        } footer: {
            Text("Every row runs in order with this edit, over the stored events. Counts compare with no rows at all; nothing is saved.")
        }
    }

    private func changeLine(_ c: DeliveryPreview.Change) -> String {
        var parts = [c.source, c.applied ?? "?"]
        if let a = c.levelBefore, let b = c.levelAfter, a != b { parts.append("\(a) → \(b)") }
        if c.badgeBefore != c.badgeAfter { parts.append(c.badgeAfter ? "joins the badge" : "leaves the badge") }
        if let r = c.refused { parts.append("kept by the \(r)") }
        return parts.joined(separator: " · ")
    }

    // MARK: Fields

    static let levels = ["passive", "active", "time-sensitive", "critical"]

    private func field(_ title: String, _ path: WritableKeyPath<DeliveryMatch, String?>, prompt: String) -> some View {
        TextField(title, text: Binding(get: { input.match[keyPath: path] ?? "" }, set: { input.match[keyPath: path] = $0 }),
                  prompt: Text(prompt))
            .autocorrectionDisabled()
            #if os(iOS)
            .textInputAutocapitalization(.never)
            #endif
    }

    private func levelBinding(_ level: String) -> Binding<Bool> {
        Binding(get: { input.match.level?.contains(level) ?? false }, set: { on in
            var l = input.match.level ?? []
            l.removeAll { $0 == level }
            if on { l.append(level) }
            input.match.level = Self.levels.filter(l.contains)
        })
    }

    // MARK: Server

    private func runPreview() async {
        guard input.problem == nil, let client = try? model.client() else {
            preview = nil
            previewError = input.problem == nil ? nil : "Fix the row to see a preview."
            return
        }
        do {
            preview = try await client.previewDelivery(DeliveryPreviewInput(settings: proposed.rows, days: 30, limit: 200))
            previewError = nil
        } catch {
            preview = nil
            previewError = error.localizedDescription
        }
    }

    private func save() async {
        saving = true
        defer { saving = false }
        let ok = await store.write(model) { client in
            switch target {
            case .row(let r): try await client.updateDeliverySetting(id: r.id, input)
            case .new: try await client.createDeliverySetting(input)
            }
        }
        if ok { dismiss() } else { saveError = store.error }
    }
}
