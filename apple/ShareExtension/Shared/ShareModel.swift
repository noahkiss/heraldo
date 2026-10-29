import Foundation
import HeraldoKit
import Observation
import UniformTypeIdentifiers
#if os(iOS)
import UIKit
#else
import AppKit
#endif

/// The share sheet's state: what was shared, the pickers' choices, and the
/// send. iOS and macOS share it; each platform's view controller hosts
/// `ShareView` and hands it the extension context.
@MainActor @Observable
final class ShareModel {
    var draft = ShareDraft()
    /// Placeholder for the title field: what goes out when it stays empty.
    var titleHint = ""
    var projects: [String] = []
    var incidents: [Event] = []
    var loading = true
    var sending = false
    var error: String?
    /// Items the server would refuse, named for the sheet.
    var skipped: [String] = []

    private let prefs = SharePrefs()
    private var client: HeraldoClient?
    private let finish: (Bool) -> Void

    init(finish: @escaping (Bool) -> Void) {
        self.finish = finish
    }

    var selectedIncident: Event? {
        draft.incidentKey.flatMap { k in incidents.first { $0.key == k } }
    }

    var canSend: Bool { client != nil && !sending && !loading }

    func load(_ items: [NSExtensionItem]) async {
        defer { loading = false }
        do {
            let c = try HeraldoClient(credentials: CredentialStore().load())
            guard c.writeToken != nil else { throw HeraldoError.notConfigured("The write token") }
            client = c
        } catch {
            self.error = "Open Heraldo and set the server and write token first. (\(error.localizedDescription))"
        }
        await read(items)
        titleHint = draft.resolvedTitle
        draft.project = prefs.lastProject
        guard let client else { return }
        async let p = try? client.projects()
        async let s = try? client.state()
        var names = (await p)?.projects.map(\.name) ?? []
        if let last = prefs.lastProject, !last.isEmpty, !names.contains(last) { names.insert(last, at: 0) }
        projects = names
        incidents = (await s)?.sortedIncidents.filter { $0.key != nil } ?? []
    }

    func send() async {
        guard let client else { return }
        sending = true
        error = nil
        defer { sending = false }
        do {
            try await ShareSender.send(draft, with: client, source: ShareSender.source(device: Self.deviceName))
            if draft.incidentKey == nil { prefs.lastProject = draft.project }
            finish(true)
        } catch {
            self.error = error.localizedDescription
        }
    }

    func cancel() { finish(false) }

    func removeFile(_ f: ShareFile) { draft.files.removeAll { $0.id == f.id } }

    static var deviceName: String {
        #if os(iOS)
        UIDevice.current.name
        #else
        Host.current().localizedName ?? "Mac"
        #endif
    }

    // MARK: Reading the shared items

    private func read(_ items: [NSExtensionItem]) async {
        var texts: [String] = []
        for item in items {
            for provider in item.attachments ?? [] {
                do {
                    try await read(provider, texts: &texts)
                } catch {
                    skipped.append(error.localizedDescription)
                }
            }
            if draft.title.isEmpty, let t = item.attributedContentText?.string.trimmingCharacters(in: .whitespacesAndNewlines),
               !t.isEmpty, !texts.contains(t), draft.url != nil {
                // Safari hands the page title here alongside the link.
                draft.title = String(t.prefix(ShareLimits.maxTitle))
            }
        }
        draft.text = texts.joined(separator: "\n\n")
        if draft.files.count > ShareLimits.maxFiles {
            skipped.append(ShareError.tooManyFiles(draft.files.count).localizedDescription)
            draft.files = Array(draft.files.prefix(ShareLimits.maxFiles))
        }
    }

    private func read(_ p: NSItemProvider, texts: inout [String]) async throws {
        if p.hasItemConformingToTypeIdentifier(UTType.fileURL.identifier),
           let url = try await p.loadItem(forTypeIdentifier: UTType.fileURL.identifier) as? URL {
            let scoped = url.startAccessingSecurityScopedResource()
            defer { if scoped { url.stopAccessingSecurityScopedResource() } }
            let type = (try? url.resourceValues(forKeys: [.contentTypeKey]).contentType)
                ?? UTType(filenameExtension: url.pathExtension) ?? .data
            let data = try Data(contentsOf: url)
            draft.files.append(try ShareFile.make(name: url.lastPathComponent, data: data, type: type))
            return
        }
        if p.hasItemConformingToTypeIdentifier(UTType.url.identifier),
           let url = try await p.loadItem(forTypeIdentifier: UTType.url.identifier) as? URL {
            if draft.url == nil, ["http", "https"].contains(url.scheme?.lowercased() ?? "") {
                draft.url = url
            } else {
                texts.append(url.absoluteString)
            }
            return
        }
        // Image, PDF or JSON data, such as a screenshot from the markup editor.
        if let typeID = p.registeredTypeIdentifiers.first(where: { id in
            guard let t = UTType(id) else { return false }
            return !t.conforms(to: .plainText) && ShareLimits.contentType(for: t) != nil
        }), let type = UTType(typeID) {
            let data = try await loadData(p, typeID)
            let ext = type.preferredFilenameExtension.map { ".\($0)" } ?? ""
            let name = (p.suggestedName ?? "file") + (p.suggestedName?.contains(".") == true ? "" : ext)
            draft.files.append(try ShareFile.make(name: name, data: data, type: type))
            return
        }
        if p.hasItemConformingToTypeIdentifier(UTType.plainText.identifier) {
            let item = try await p.loadItem(forTypeIdentifier: UTType.plainText.identifier)
            if let s = item as? String {
                texts.append(s)
            } else if let d = item as? Data, let s = String(data: d, encoding: .utf8) {
                texts.append(s)
            }
            return
        }
        throw ShareError.unsupported(p.suggestedName ?? p.registeredTypeIdentifiers.first ?? "An item")
    }

    private func loadData(_ p: NSItemProvider, _ typeID: String) async throws -> Data {
        let name = p.suggestedName ?? "An item"
        return try await withCheckedThrowingContinuation { cont in
            _ = p.loadDataRepresentation(forTypeIdentifier: typeID) { data, error in
                if let data { cont.resume(returning: data) } else {
                    cont.resume(throwing: error ?? ShareError.empty(name))
                }
            }
        }
    }
}
