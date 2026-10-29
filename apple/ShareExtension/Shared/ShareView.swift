import HeraldoKit
import SwiftUI

/// The share sheet: a title, the shared items, a project, and an optional
/// open incident to add them to. It posts one passive event.
struct ShareView: View {
    @Bindable var model: ShareModel

    var body: some View {
        #if os(iOS)
        NavigationStack {
            form
                .navigationTitle("Send to Heraldo")
                .navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) {
                        Button("Cancel") { model.cancel() }
                    }
                    ToolbarItem(placement: .confirmationAction) { sendButton }
                }
        }
        #else
        VStack(spacing: 0) {
            form.formStyle(.grouped)
            HStack {
                if model.sending { ProgressView().controlSize(.small) }
                Spacer()
                Button("Cancel", role: .cancel) { model.cancel() }
                    .keyboardShortcut(.cancelAction)
                sendButton.keyboardShortcut(.defaultAction)
            }
            .padding()
        }
        .frame(width: 440, height: 520)
        #endif
    }

    private var sendButton: some View {
        Button("Send") { Task { await model.send() } }
            .disabled(!model.canSend)
    }

    private var form: some View {
        Form {
            Section {
                TextField("Title", text: $model.draft.title, prompt: Text(model.titleHint.isEmpty ? "Title" : model.titleHint))
                TextField("Text", text: $model.draft.text, axis: .vertical)
                    .lineLimit(3...8)
            }
            if model.draft.url != nil || !model.draft.files.isEmpty {
                Section("Attached") {
                    if let url = model.draft.url {
                        Label(url.absoluteString, systemImage: "link").lineLimit(1)
                    }
                    ForEach(model.draft.files) { f in
                        HStack {
                            Label(f.name, systemImage: f.contentType.hasPrefix("image/") ? "photo" : "doc")
                                .lineLimit(1)
                            Spacer()
                            Text(ByteCountFormatter.string(fromByteCount: Int64(f.data.count), countStyle: .file))
                                .foregroundStyle(.secondary)
                            Button {
                                model.removeFile(f)
                            } label: {
                                Image(systemName: "xmark.circle.fill")
                            }
                            .buttonStyle(.borderless)
                            .accessibilityLabel("Remove \(f.name)")
                        }
                    }
                }
            }
            Section {
                Picker("Add to open incident", selection: $model.draft.incidentKey) {
                    Text("None").tag(String?.none)
                    ForEach(model.incidents, id: \.id) { e in
                        Text(e.isAcked ? "\(e.headline) (acked)" : e.headline).tag(e.key)
                    }
                }
                if let incident = model.selectedIncident {
                    LabeledContent("Project", value: incident.project ?? "None")
                } else {
                    Picker("Project", selection: $model.draft.project) {
                        Text("None").tag(String?.none)
                        ForEach(model.projects, id: \.self) { Text($0).tag(String?.some($0)) }
                    }
                }
            } footer: {
                if model.loading { Text("Loading projects and incidents…") }
            }
            if !model.skipped.isEmpty || model.error != nil {
                Section {
                    ForEach(model.skipped, id: \.self) { Text("Skipped: \($0)").foregroundStyle(.secondary) }
                    if let error = model.error { Text(error).foregroundStyle(.red) }
                }
            }
        }
    }
}
