import HeraldoKit
import SwiftUI

struct SettingsView: View {
    @Environment(AppModel.self) private var model
    @State private var serverURL = ""
    @State private var readToken = ""
    @State private var writeToken = ""
    @State private var adminToken = ""
    @State private var saving = false
    @State private var result: String?
    @State private var failed = false
    #if os(macOS)
    @AppStorage(DockPolicy.key) private var dockOnlyWithWindow = true
    #endif

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("https://heraldo.example.com", text: $serverURL)
                        .textContentType(.URL)
                        .autocorrectionDisabled()
                        #if os(iOS)
                        .keyboardType(.URL)
                        .textInputAutocapitalization(.never)
                        #endif
                    SecureField("Read token", text: $readToken)
                    SecureField("Write token", text: $writeToken)
                    SecureField("Admin token (optional)", text: $adminToken)
                    Button {
                        Task { await save() }
                    } label: {
                        if saving { ProgressView() } else { Text("Save and test") }
                    }
                    .disabled(saving || serverURL.isEmpty || readToken.isEmpty)
                    if let result {
                        Text(result).font(.footnote).foregroundStyle(failed ? .red : .secondary)
                    }
                } header: {
                    Text("Server")
                } footer: {
                    Text("The read token syncs history and registers this device. The write token acknowledges incidents and sends events. The admin token changes server settings. Saved in \(model.store.modeDescription).")
                }

                PushSection()

                DisplaySection()

                ServerSettingsSection()

                GuidanceSection()

                MutesSection()

                #if os(macOS)
                LoginItemSection()

                Section("Dock") {
                    Toggle("Show in Dock only while a window is open", isOn: $dockOnlyWithWindow)
                }
                #endif

                Section("Sync") {
                    LabeledContent("Events kept", value: "\(model.cache.events.count)")
                    LabeledContent("Unread", value: "\(model.unread.inboxCount)")
                    LabeledContent("Badge", value: "\(model.unread.badge)")
                    if let last = model.lastSync {
                        LabeledContent("Last sync") { Text(last, format: .relative(presentation: .named)) }
                    }
                    if let error = model.lastError {
                        Text(error).font(.footnote).foregroundStyle(.red)
                    }
                    Button("Sync now") { Task { await AppSync.shared.syncNow() } }
                        .disabled(!model.isConfigured || model.isSyncing)
                }

                Section {
                    LabeledContent("Version", value: AppInfo.version())
                }
            }
            .formStyle(.grouped)
            .navigationTitle("Settings")
        }
        .onAppear(perform: load)
    }

    private func load() {
        serverURL = model.credentials.serverURL ?? ""
        readToken = model.credentials.readToken ?? ""
        writeToken = model.credentials.writeToken ?? ""
        adminToken = model.credentials.adminToken ?? ""
    }

    private func save() async {
        saving = true
        defer { saving = false }
        do {
            let h = try await model.save(Credentials(serverURL: serverURL, readToken: readToken, writeToken: writeToken,
                                                       adminToken: adminToken))
            failed = false
            result = "Connected: heraldo \(h.version), push \(h.push ?? "unknown")."
        } catch {
            failed = true
            result = error.localizedDescription
        }
    }
}

/// The mutes in force, with End.
struct MutesSection: View {
    @Environment(AppModel.self) private var model
    @State private var mutes: [Mute] = []
    @State private var error: String?

    var body: some View {
        Section("Mutes") {
            if mutes.isEmpty {
                Text("None").foregroundStyle(.secondary)
            }
            ForEach(mutes) { m in
                HStack {
                    VStack(alignment: .leading) {
                        Text(m.key ?? m.project ?? m.source ?? m.id)
                        if m.key != nil || m.project != nil, let source = m.source {
                            Text(source).font(.caption).foregroundStyle(.secondary)
                        }
                    }
                    Spacer()
                    Text(m.until, format: .relative(presentation: .named)).font(.caption).foregroundStyle(.secondary)
                    Button("End") { Task { await end(m) } }
                        .disabled(model.credentials.writeToken?.isEmpty ?? true)
                }
            }
            if let error {
                Text(error).font(.footnote).foregroundStyle(.red)
            }
        }
        .task(id: model.lastSync) { await load() }
    }

    private func load() async {
        guard model.isConfigured, let client = try? model.client(), client.writeToken != nil else { return }
        do {
            mutes = try await client.mutes()
            error = nil
        } catch let e as HeraldoError where e.status == 404 {
            mutes = [] // a server before stage 5
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func end(_ m: Mute) async {
        do {
            try await model.client().endMute(id: m.id)
            await load()
        } catch {
            self.error = error.localizedDescription
        }
    }
}
