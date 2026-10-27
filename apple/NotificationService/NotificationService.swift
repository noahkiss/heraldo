import HeraldoKit
import UniformTypeIdentifiers
import UserNotifications

/// Rewrites a push before it is shown: fetches the full event by `heraldo.id`
/// with the read token the app shares through the app group Keychain, puts
/// in the full text when the push carried a cut copy, and attaches the image,
/// else the source's icon, as the thumbnail. A digest keeps its one line; the
/// full text is in the app.
final class NotificationService: UNNotificationServiceExtension {
    private var contentHandler: ((UNNotificationContent) -> Void)?
    private var bestAttempt: UNMutableNotificationContent?
    private var work: Task<Void, Never>?
    private let lock = NSLock()

    /// The longest body a notification shows; iOS cuts the rest anyway.
    static let maxBody = 2000

    override func didReceive(
        _ request: UNNotificationRequest,
        withContentHandler contentHandler: @escaping (UNNotificationContent) -> Void
    ) {
        self.contentHandler = contentHandler
        guard let content = request.content.mutableCopy() as? UNMutableNotificationContent,
              let info = PushInfo(userInfo: request.content.userInfo) else {
            contentHandler(request.content)
            return
        }
        bestAttempt = content
        work = Task {
            let final = await Self.enrich(content, info: info)
            self.deliver(final)
        }
    }

    override func serviceExtensionTimeWillExpire() {
        work?.cancel()
        if let bestAttempt { deliver(bestAttempt) }
    }

    /// Calls the handler once: from the fetch, or from the expiry, whichever is first.
    private func deliver(_ content: UNNotificationContent) {
        lock.lock()
        let handler = contentHandler
        contentHandler = nil
        lock.unlock()
        handler?(content)
    }

    static func enrich(_ content: UNMutableNotificationContent, info: PushInfo) async -> UNNotificationContent {
        let creds = CredentialStore().load()
        let client = try? HeraldoClient(credentials: creds, session: session)
        let event = try? await client?.event(id: info.id)

        if let event {
            if info.truncated, info.kind != "digest", let body = event.body, !body.isEmpty {
                let text = Markdown.plainText(body)
                content.body = text.count > maxBody ? String(text.prefix(maxBody)) + "…" : text
            }
            // thread-id comes from the payload; keep it, fill it only when missing.
            if content.threadIdentifier.isEmpty { content.threadIdentifier = event.thread ?? event.source }
        }

        let imageURL = (info.image ?? event?.image).flatMap(URL.init(string:))
        if let imageURL, !Task.isCancelled, let attachment = await attachment(from: imageURL, id: "image") {
            content.attachments = [attachment]
        } else if !Task.isCancelled, let icon = await icon(info, client: client) {
            content.attachments = [icon]
        }
        return content
    }

    /// The source's icon: the registry icon by name, with the read token,
    /// else an https icon URL.
    private static func icon(_ info: PushInfo, client: HeraldoClient?) async -> UNNotificationAttachment? {
        if let name = info.icon, let client, let (data, mime) = try? await client.icon(name: name) {
            return attachment(data: data, type: mime.flatMap { UTType(mimeType: $0) } ?? .png, id: "icon")
        }
        if let url = info.iconURL.flatMap(URL.init(string:)), url.scheme == "https" {
            return await attachment(from: url, id: "icon")
        }
        return nil
    }

    private static let session: URLSession = {
        let c = URLSessionConfiguration.ephemeral
        c.timeoutIntervalForRequest = 10
        c.timeoutIntervalForResource = 20
        return URLSession(configuration: c)
    }()

    private static func download(_ url: URL) async -> (data: Data, type: UTType?)? {
        guard let (data, response) = try? await session.data(from: url),
              let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode), !data.isEmpty else {
            return nil
        }
        let type = http.mimeType.flatMap { UTType(mimeType: $0) } ?? UTType(filenameExtension: url.pathExtension)
        return (data, type)
    }

    /// The image as a thumbnail attachment, in a file with the right extension.
    private static func attachment(from url: URL, id: String) async -> UNNotificationAttachment? {
        guard let (data, type) = await download(url) else { return nil }
        return attachment(data: data, type: type, id: id,
                          fallbackExtension: url.pathExtension.isEmpty ? "png" : url.pathExtension)
    }

    private static func attachment(data: Data, type: UTType?, id: String,
                                   fallbackExtension: String = "png") -> UNNotificationAttachment? {
        let ext = type?.preferredFilenameExtension ?? fallbackExtension
        let file = FileManager.default.temporaryDirectory.appending(path: "\(UUID().uuidString).\(ext)")
        do {
            try data.write(to: file)
            var options: [String: Any] = [:]
            if let type { options[UNNotificationAttachmentOptionsTypeHintKey] = type.identifier }
            return try UNNotificationAttachment(identifier: id, url: file, options: options)
        } catch {
            return nil
        }
    }
}
