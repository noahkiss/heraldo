import Foundation
import ImageIO
import UniformTypeIdentifiers

/// Share in: what the share extensions and the Shortcuts action send. Both
/// build a `ShareDraft`, then `ShareSender` uploads its files and posts one
/// passive event.

/// The server's attachment limits (docs/api.md, "Attachments").
public enum ShareLimits {
    /// One file, after any image shrink.
    public static let maxBytes = 5 << 20
    /// Files on one event.
    public static let maxFiles = 8
    /// Body text above this goes up as a text file instead.
    public static let maxBodyBytes = 256 << 10
    public static let maxTitle = 256

    /// The MIME type the server stores for `type`, or nil when it refuses
    /// the type. SVG reads as text and is refused, so it is too.
    public static func contentType(for type: UTType) -> String? {
        if type.conforms(to: .svg) { return nil }
        if type.conforms(to: .image) { return type.preferredMIMEType ?? "image/png" }
        if type.conforms(to: .pdf) { return "application/pdf" }
        if type.conforms(to: .json) { return "application/json" }
        if type.conforms(to: .plainText) { return "text/plain" }
        return nil
    }
}

/// One file to attach.
public struct ShareFile: Hashable, Sendable, Identifiable {
    public var id = UUID()
    public var name: String
    public var data: Data
    public var contentType: String

    public init(name: String, data: Data, contentType: String) {
        self.name = name
        self.data = data
        self.contentType = contentType
    }

    /// A file of `type`, with an image over the limit shrunk to a JPEG that
    /// fits. Throws for a type the server refuses or a file that does not fit.
    public static func make(name: String, data: Data, type: UTType) throws -> ShareFile {
        guard let mime = ShareLimits.contentType(for: type) else { throw ShareError.unsupported(name) }
        if data.isEmpty { throw ShareError.empty(name) }
        if data.count <= ShareLimits.maxBytes { return ShareFile(name: name, data: data, contentType: mime) }
        if mime.hasPrefix("image/"), let small = ShareImages.fit(data, maxBytes: ShareLimits.maxBytes) {
            return ShareFile(name: name, data: small, contentType: "image/jpeg")
        }
        throw ShareError.tooLarge(name, data.count)
    }
}

public enum ShareError: LocalizedError, Equatable {
    case unsupported(String)
    case tooLarge(String, Int)
    case empty(String)
    case tooManyFiles(Int)
    case nothingToSend

    public var errorDescription: String? {
        switch self {
        case .unsupported(let n): return "\(n): heraldo stores images, plain text, PDF and JSON only."
        case .tooLarge(let n, let b):
            return "\(n) is \(ByteCountFormatter.string(fromByteCount: Int64(b), countStyle: .file)); the limit is 5 MB."
        case .empty(let n): return "\(n) is empty."
        case .tooManyFiles(let n): return "\(n) files; an event takes \(ShareLimits.maxFiles) at most."
        case .nothingToSend: return "Nothing to send: add a title, text, a link or a file."
        }
    }
}

/// The share sheet's content and choices.
public struct ShareDraft: Sendable {
    public var title = ""
    public var text = ""
    /// A shared web link: the event's `url`, opened on tap.
    public var url: URL?
    public var files: [ShareFile] = []
    /// Ignored when `incidentKey` is set: the incident's key files the event.
    public var project: String?
    /// Post as `kind incident, action update` on this key.
    public var incidentKey: String?
    public var level = "passive"
    /// One id per draft, so a retried send stores the event once.
    public var dedup = "share-\(UUID().uuidString.lowercased())"

    public init(title: String = "", text: String = "", url: URL? = nil, files: [ShareFile] = [],
                project: String? = nil, incidentKey: String? = nil) {
        self.title = title
        self.text = text
        self.url = url
        self.files = files
        self.project = project
        self.incidentKey = incidentKey
    }

    /// The title to send: the typed one, else the first line of the text,
    /// else the link's host, else the first file's name.
    public var resolvedTitle: String {
        let typed = title.trimmingCharacters(in: .whitespacesAndNewlines)
        let firstLine = text.split(whereSeparator: \.isNewline).first.map {
            $0.trimmingCharacters(in: .whitespaces)
        } ?? ""
        let pick = [typed, firstLine, url?.host() ?? "", files.first?.name ?? ""].first { !$0.isEmpty } ?? "Shared"
        return String(pick.prefix(ShareLimits.maxTitle))
    }

    /// The body: the text, with the link added when the text lacks it.
    public var resolvedBody: String {
        var body = text.trimmingCharacters(in: .whitespacesAndNewlines)
        if let url, !body.contains(url.absoluteString) {
            body = body.isEmpty ? url.absoluteString : body + "\n\n" + url.absoluteString
        }
        return body
    }

    /// Checks the draft against the limits. A body over the limit moves
    /// into a text file, so it still arrives whole.
    public func checked() throws -> ShareDraft {
        var d = self
        if d.resolvedBody.utf8.count > ShareLimits.maxBodyBytes {
            d.files.insert(try ShareFile.make(name: "text.txt", data: Data(d.resolvedBody.utf8), type: .plainText), at: 0)
            d.text = String(d.text.prefix(2000))
        }
        if d.files.count > ShareLimits.maxFiles { throw ShareError.tooManyFiles(d.files.count) }
        if d.title.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty, d.text.isEmpty, d.url == nil, d.files.isEmpty {
            throw ShareError.nothingToSend
        }
        return d
    }

    /// The event to post, once the files are stored as `attachmentIDs`.
    public func input(source: String, attachmentIDs: [String]) -> EventInput {
        let body = resolvedBody
        var e = EventInput(source: source, title: resolvedTitle, body: body.isEmpty ? nil : body, level: level,
                           url: url.flatMap { ["http", "https"].contains($0.scheme?.lowercased() ?? "") ? $0.absoluteString : nil },
                           attachments: attachmentIDs.isEmpty ? nil : attachmentIDs, dedup: dedup)
        if let key = incidentKey, !key.isEmpty {
            e.kind = "incident"
            e.action = "update"
            e.key = key
        } else if let p = project, !p.isEmpty, p != noProject {
            e.project = p
        }
        return e
    }
}

public enum ShareSender {
    /// `share:<device>`: the device name in lower case, with every run of
    /// other characters as one dash.
    public static func source(device: String) -> String {
        var slug = ""
        for ch in device.lowercased() {
            if ch.isASCII && (ch.isLetter || ch.isNumber) {
                slug.append(ch)
            } else if !slug.isEmpty, slug.last != "-" {
                slug.append("-")
            }
        }
        while slug.hasSuffix("-") { slug.removeLast() }
        return "share:" + String((slug.isEmpty ? "device" : slug).prefix(64))
    }

    /// Uploads the draft's files, then posts the event. Returns it.
    @discardableResult
    public static func send(_ draft: ShareDraft, with client: HeraldoClient, source: String) async throws -> Event {
        let d = try draft.checked()
        var ids: [String] = []
        for f in d.files {
            ids.append(try await client.upload(f.data, contentType: f.contentType).id)
        }
        return try await client.post(d.input(source: source, attachmentIDs: ids))
    }
}

/// Shrinks an image to a JPEG under a byte limit.
public enum ShareImages {
    public static func fit(_ data: Data, maxBytes: Int) -> Data? {
        guard let src = CGImageSourceCreateWithData(data as CFData, nil) else { return nil }
        for side in [4096, 3072, 2048, 1536, 1024] {
            let opts: [CFString: Any] = [
                kCGImageSourceCreateThumbnailFromImageAlways: true,
                kCGImageSourceCreateThumbnailWithTransform: true,
                kCGImageSourceThumbnailMaxPixelSize: side,
            ]
            guard let img = CGImageSourceCreateThumbnailAtIndex(src, 0, opts as CFDictionary) else { return nil }
            let out = NSMutableData()
            guard let dst = CGImageDestinationCreateWithData(out, UTType.jpeg.identifier as CFString, 1, nil) else { return nil }
            CGImageDestinationAddImage(dst, img, [kCGImageDestinationLossyCompressionQuality: 0.8] as CFDictionary)
            guard CGImageDestinationFinalize(dst) else { return nil }
            if out.length <= maxBytes { return out as Data }
        }
        return nil
    }
}

/// The share sheet's remembered choice, in the app group's defaults so the
/// extensions and the app see one value. A build without an app group keeps
/// it per process.
public struct SharePrefs: Sendable {
    static let lastProjectKey = "share.lastProject"
    let suite: String?

    public init(bundle: Bundle = .main) {
        let g = (bundle.object(forInfoDictionaryKey: CredentialStore.accessGroupInfoKey) as? String)?
            .trimmingCharacters(in: .whitespaces) ?? ""
        suite = g.isEmpty || g.hasPrefix("$(") ? nil : g
    }

    public init(suite: String?) { self.suite = suite }

    private var defaults: UserDefaults { suite.flatMap(UserDefaults.init(suiteName:)) ?? .standard }

    public var lastProject: String? {
        get { defaults.string(forKey: Self.lastProjectKey) }
        nonmutating set { defaults.set(newValue, forKey: Self.lastProjectKey) }
    }
}
