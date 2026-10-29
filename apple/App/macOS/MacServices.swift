import AppKit
import HeraldoKit

/// The Services menu entry "Send to Heraldo": the selected text or link in
/// any app goes to the server as one passive event, under the project the
/// share sheet used last. Info.plist's NSServices names `sendToHeraldo`.
final class MacServices: NSObject {
    static let shared = MacServices()

    @objc func sendToHeraldo(_ pboard: NSPasteboard, userData: String?,
                             error: AutoreleasingUnsafeMutablePointer<NSString?>) {
        var draft = ShareDraft()
        if let url = pboard.readObjects(forClasses: [NSURL.self])?.first as? URL,
           ["http", "https"].contains(url.scheme?.lowercased() ?? "") {
            draft.url = url
        }
        if let text = pboard.string(forType: .string), text != draft.url?.absoluteString {
            draft.text = text
        }
        guard !draft.text.isEmpty || draft.url != nil else {
            error.pointee = "Nothing to send: select text or a link." as NSString
            return
        }
        draft.project = SharePrefs().lastProject
        let client: HeraldoClient
        do {
            client = try HeraldoClient(credentials: CredentialStore().load())
        } catch let e {
            error.pointee = e.localizedDescription as NSString
            return
        }
        let source = ShareSender.source(device: Host.current().localizedName ?? "Mac")
        Task { @MainActor in
            do {
                try await ShareSender.send(draft, with: client, source: source)
                await AppModel.shared.refresh()
            } catch {
                let alert = NSAlert()
                alert.messageText = "Send to Heraldo failed"
                alert.informativeText = error.localizedDescription
                alert.runModal()
            }
        }
    }
}
