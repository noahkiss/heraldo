import HeraldoKit
import Intents
import UniformTypeIdentifiers
import UserNotifications

/// Rewrites a push before it is shown: fetches the full event by `heraldo.id`
/// with the read token the app shares through the app group Keychain, puts
/// in the full text when the push carried a cut copy, attaches the image, and
/// shows the source's icon as a sender avatar when the event has one.
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
            if info.truncated, let body = event.body, !body.isEmpty {
                let text = Markdown.plainText(body)
                content.body = text.count > maxBody ? String(text.prefix(maxBody)) + "…" : text
            }
            // thread-id comes from the payload; keep it, fill it only when missing.
            if content.threadIdentifier.isEmpty { content.threadIdentifier = event.thread ?? event.source }
        }

        let imageURL = (info.image ?? event?.image).flatMap(URL.init(string:))
        if let imageURL, !Task.isCancelled, let attachment = await attachment(from: imageURL, id: "image") {
            content.attachments = [attachment]
        }

        if let event, let icon = event.icon.flatMap(URL.init(string:)), icon.scheme?.hasPrefix("http") == true,
           !Task.isCancelled, let avatar = await download(icon) {
            if let updated = communication(content, event: event, avatar: avatar.data) {
                return updated
            }
        }
        return content
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
        let ext = type?.preferredFilenameExtension ?? (url.pathExtension.isEmpty ? "png" : url.pathExtension)
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

    /// The communication notification style: the source as the sender, with
    /// its icon as the avatar. Needs the communication notifications
    /// entitlement on the app; without it iOS refuses and the plain content stays.
    private static func communication(_ content: UNMutableNotificationContent, event: Event,
                                      avatar: Data) -> UNNotificationContent? {
        let handle = INPersonHandle(value: event.source, type: .unknown)
        let sender = INPerson(personHandle: handle, nameComponents: nil, displayName: event.source,
                              image: INImage(imageData: avatar), contactIdentifier: nil,
                              customIdentifier: "source:\(event.source)")
        let intent = INSendMessageIntent(recipients: nil, outgoingMessageType: .outgoingMessageText,
                                         content: content.body, speakableGroupName: nil,
                                         conversationIdentifier: content.threadIdentifier,
                                         serviceName: nil, sender: sender, attachments: nil)
        intent.setImage(INImage(imageData: avatar), forParameterNamed: \.sender)
        let interaction = INInteraction(intent: intent, response: nil)
        interaction.direction = .incoming
        interaction.donate(completion: nil)
        return try? content.updating(from: intent)
    }
}
