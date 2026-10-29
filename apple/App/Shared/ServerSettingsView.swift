import HeraldoKit
import SwiftUI

/// Quiet hours, the badge and emergency escalation: server settings, so
/// every device and the CLI see the same values. The admin token changes them; without it they are
/// read-only. A key a server flag or env var sets is read-only too.
struct ServerSettingsSection: View {
    @Environment(AppModel.self) private var model
    @State private var settings: [String: ServerSetting] = [:]
    @State private var loaded = false
    @State private var error: String?
    @State private var tz = ""
    @State private var projects = ""
    @State private var retry = ""
    @State private var expire = ""
    @State private var pushoverToken = ""
    @State private var pushoverUser = ""

    private var canEdit: Bool { !(model.credentials.adminToken ?? "").isEmpty }

    var body: some View {
        Group {
            Section {
                if !loaded {
                    Text(model.isConfigured ? "Loading" : "Not connected").foregroundStyle(.secondary)
                } else {
                    Toggle("Quiet hours", isOn: boolBinding("quiet.enabled"))
                        .disabled(!editable("quiet.enabled"))
                    clock("Start", "quiet.start")
                    clock("End", "quiet.end")
                    HStack {
                        TextField("Time zone", text: $tz)
                            .autocorrectionDisabled()
                            #if os(iOS)
                            .textInputAutocapitalization(.never)
                            #endif
                            .onSubmit { put("quiet.tz", .string(tz.trimmingCharacters(in: .whitespaces))) }
                            .disabled(!editable("quiet.tz"))
                        if editable("quiet.tz"), tz != TimeZone.current.identifier {
                            Button("Use \(TimeZone.current.identifier)") {
                                tz = TimeZone.current.identifier
                                put("quiet.tz", .string(tz))
                            }
                        }
                    }
                    picker("Lowest level that still alerts", "quiet.floor", labels: Self.levelNames)
                }
            } header: {
                Text("Quiet hours")
            } footer: {
                Text("During quiet hours, alerts below this level arrive silently. Critical always alerts.")
            }

            Section {
                if loaded {
                    picker("Badge counts", "badge.scope", labels: Self.scopeNames)
                    TextField("Projects (all when empty)", text: $projects)
                        .autocorrectionDisabled()
                        #if os(iOS)
                        .textInputAutocapitalization(.never)
                        #endif
                        .onSubmit { put("badge.projects", .list(Self.split(projects))) }
                        .disabled(!editable("badge.projects"))
                }
                if let error {
                    Text(error).font(.footnote).foregroundStyle(.red)
                }
            } header: {
                Text("Badge")
            } footer: {
                Text(canEdit ? "Applies to every device." : "Read-only without an admin token.")
            }

            Section {
                if loaded {
                    Toggle("Escalate critical alerts to Pushover", isOn: boolBinding("escalation.enabled"))
                        .disabled(!editable("escalation.enabled"))
                    duration("Repeat every", "escalation.retry", text: $retry)
                    duration("Stop repeating after", "escalation.expire", text: $expire)
                    secret("Pushover app token", "escalation.pushover.token", text: $pushoverToken)
                    secret("Pushover user key", "escalation.pushover.user", text: $pushoverUser)
                }
            } header: {
                Text("Emergency escalation")
            } footer: {
                Text("Pushover repeats a critical alert until it is acknowledged. Acknowledging it in Heraldo stops it.")
            }
        }
        .task(id: model.credentials) { await load() }
    }

    // MARK: Rows

    private func clock(_ title: String, _ key: String) -> some View {
        DatePicker(title, selection: Binding(
            get: { Self.date(settings[key]?.value?.string ?? "00:00") },
            set: { put(key, .string(Self.clock($0))) }
        ), displayedComponents: .hourAndMinute)
        .disabled(!editable(key))
    }

    /// A Go duration, such as 1m or 2h, sent on submit.
    private func duration(_ title: String, _ key: String, text: Binding<String>) -> some View {
        LabeledContent(title) {
            TextField("1m", text: text)
                .multilineTextAlignment(.trailing)
                .autocorrectionDisabled()
                #if os(iOS)
                .textInputAutocapitalization(.never)
                #endif
                .onSubmit { put(key, .string(text.wrappedValue.trimmingCharacters(in: .whitespaces))) }
                .disabled(!editable(key))
        }
    }

    /// A write-only secret: the field never shows the stored value, and
    /// empties after a save.
    private func secret(_ title: String, _ key: String, text: Binding<String>) -> some View {
        SecureField(title, text: text, prompt: Text(settings[key]?.set == true ? "Set" : "Not set"))
            .onSubmit {
                let v = text.wrappedValue.trimmingCharacters(in: .whitespacesAndNewlines)
                text.wrappedValue = ""
                if !v.isEmpty { putSecret(key, v) }
            }
            .disabled(!editable(key))
    }

    private func picker(_ title: String, _ key: String, labels: [String: String]) -> some View {
        Picker(title, selection: Binding(
            get: { settings[key]?.value?.string ?? "" },
            set: { put(key, .string($0)) }
        )) {
            ForEach(settings[key]?.enum ?? [], id: \.self) { v in
                Text(labels[v] ?? v).tag(v)
            }
        }
        .disabled(!editable(key))
    }

    private func boolBinding(_ key: String) -> Binding<Bool> {
        Binding(get: { settings[key]?.value?.bool ?? false }, set: { put(key, .bool($0)) })
    }

    private func editable(_ key: String) -> Bool {
        canEdit && settings[key].map { !$0.isLocked } ?? false
    }

    static let levelNames = [
        "passive": "Passive", "active": "Active", "time-sensitive": "Time Sensitive", "critical": "Critical",
    ]
    static let scopeNames = [
        "attention": "Needs attention", "pushed": "Unread alerts", "none": "Nothing",
    ]

    // MARK: Server

    private func load() async {
        guard model.isConfigured, let client = try? model.client() else { loaded = false; return }
        do {
            guard let all = try await client.settings() else {
                error = "This server has no settings."
                return
            }
            settings = Dictionary(uniqueKeysWithValues: all.filter {
                $0.key.hasPrefix("quiet.") || $0.key.hasPrefix("badge.") || $0.key.hasPrefix("escalation.")
            }.map { ($0.key, $0) })
            retry = settings["escalation.retry"]?.value?.string ?? ""
            expire = settings["escalation.expire"]?.value?.string ?? ""
            tz = settings["quiet.tz"]?.value?.string ?? ""
            projects = (settings["badge.projects"]?.value?.list ?? []).joined(separator: ", ")
            loaded = true
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func put(_ key: String, _ value: SettingValue) {
        guard editable(key), settings[key]?.value != value else { return }
        Task {
            do {
                settings[key] = try await model.client().putSetting(key, value)
                error = nil
                if key.hasPrefix("badge.") { await model.refresh() }
            } catch {
                self.error = error.localizedDescription
                await load()
            }
        }
    }

    /// A secret's value is never read back, so it always writes.
    private func putSecret(_ key: String, _ value: String) {
        guard editable(key) else { return }
        Task {
            do {
                settings[key] = try await model.client().putSetting(key, .string(value))
                error = nil
            } catch {
                self.error = error.localizedDescription
                await load()
            }
        }
    }

    // MARK: Values

    static func split(_ s: String) -> [String] {
        s.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
    }

    static func date(_ clock: String) -> Date {
        let parts = clock.split(separator: ":").compactMap { Int($0) }
        var c = DateComponents()
        c.hour = parts.first ?? 0
        c.minute = parts.count > 1 ? parts[1] : 0
        return Calendar.current.date(from: c) ?? Date()
    }

    static func clock(_ d: Date) -> String {
        let c = Calendar.current.dateComponents([.hour, .minute], from: d)
        return String(format: "%02d:%02d", c.hour ?? 0, c.minute ?? 0)
    }
}
