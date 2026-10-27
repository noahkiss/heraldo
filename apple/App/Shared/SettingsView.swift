import HeraldoKit
import SwiftUI

struct SettingsView: View {
    @Environment(AppModel.self) private var model
    @State private var serverURL = ""
    @State private var readToken = ""
    @State private var writeToken = ""
    @State private var saving = false
    @State private var result: String?
    @State private var failed = false

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
                    Text("The read token syncs history and registers this device. The write token acknowledges incidents and sends events. Saved in \(model.store.modeDescription).")
                }

                #if os(iOS)
                PushSection()
                #endif

                Section("Sync") {
                    LabeledContent("Events kept", value: "\(model.cache.events.count)")
                    LabeledContent("Unread", value: "\(model.unread.badge)")
                    if let last = model.lastSync {
                        LabeledContent("Last sync") { Text(last, format: .relative(presentation: .named)) }
                    }
                    if let error = model.lastError {
                        Text(error).font(.footnote).foregroundStyle(.red)
                    }
                    Button("Sync now") { Task { await model.refresh() } }
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
    }

    private func save() async {
        saving = true
        defer { saving = false }
        do {
            let h = try await model.save(Credentials(serverURL: serverURL, readToken: readToken, writeToken: writeToken))
            failed = false
            result = "Connected: heraldo \(h.version), push \(h.push ?? "unknown")."
        } catch {
            failed = true
            result = error.localizedDescription
        }
    }
}
